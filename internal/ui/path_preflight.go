package ui

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	tea "charm.land/bubbletea/v2"

	"github.com/abdul-hamid-achik/sonar/internal/agent"
)

const (
	maxPromptPathIntents     = 4
	maxPromptPathScanIntents = 32
)

func (m *Model) beginPromptPathPreflight(draft string) (tea.Cmd, bool) {
	if m == nil || m.agent == nil {
		return nil, false
	}
	scan := scanExplicitPromptPaths(draft)
	if len(scan.Intents) == 0 {
		return nil, false
	}
	m.readScopeOpToken++
	token := m.readScopeOpToken
	authority := m.presentedMode()
	m.readScopeOpRunning = true
	m.readScopeOpLabel = "Checking explicit path scopes"
	m.readScopeOpDraft = draft
	m.input.Blur()
	m.recalcViewportHeight()

	agentInstance := m.agent
	inspect := func() tea.Msg {
		if scan.MoreCandidates {
			return PromptPathPreflightResultMsg{
				Token: token, Draft: draft, Authority: authority,
				MoreCandidates: true, CandidateLimitExceeded: true,
			}
		}
		readGrants, writeGrants, unavailableWrites, tooManyGrants := inspectPromptPathGrantIntents(
			agentInstance, scan.Intents, authority != ModePlan,
		)
		return PromptPathPreflightResultMsg{
			Token: token, Draft: draft, Authority: authority, Grants: readGrants,
			WriteGrants: writeGrants, UnavailableWrites: unavailableWrites,
			MoreCandidates: tooManyGrants,
		}
	}
	return tea.Batch(m.startActivityCmd(), inspect), true
}

func (m *Model) handlePromptPathPreflightResult(msg PromptPathPreflightResultMsg) tea.Cmd {
	if !m.readScopeOpRunning || msg.Token != m.readScopeOpToken {
		releaseReadGrants(msg.Grants)
		releaseWriteGrants(msg.WriteGrants)
		return nil
	}
	m.readScopeOpRunning = false
	m.readScopeOpLabel = ""
	m.readScopeOpDraft = ""
	if m.shuttingDown {
		releaseReadGrants(msg.Grants)
		releaseWriteGrants(msg.WriteGrants)
		return nil
	}
	// The preflight receipt may install an external-scope decision or restore
	// and submit the held draft. Search can have opened while the filesystem
	// inspection was in flight, so release it before either path changes focus
	// or footer authority.
	m.preemptTranscriptSearch()
	if msg.MoreCandidates {
		releaseReadGrants(msg.Grants)
		releaseWriteGrants(msg.WriteGrants)
		m.input.SetValue(msg.Draft)
		m.input.CursorEnd()
		m.input.Focus()
		_ = m.reflowInputViewport()
		guidance := "External path preflight requires more than 4 new temporary scopes. Split the request into smaller groups; no path was authorized and nothing was sent."
		if msg.CandidateLimitExceeded {
			guidance = "External path preflight found more than 32 distinct path candidates. Split the request into smaller groups; no path was authorized and nothing was sent."
		}
		m.entries = append(m.entries, ChatEntry{
			Kind:    "error",
			Content: guidance,
		})
		m.recalcViewportHeight()
		m.refreshTranscript()
		m.gotoBottomIfFollowing()
		return nil
	}
	if len(msg.UnavailableWrites) > 0 {
		releaseReadGrants(msg.Grants)
		releaseWriteGrants(msg.WriteGrants)
		m.input.SetValue(msg.Draft)
		m.input.CursorEnd()
		m.input.Focus()
		_ = m.reflowInputViewport()
		paths := make([]string, 0, len(msg.UnavailableWrites))
		for _, path := range msg.UnavailableWrites {
			paths = append(paths, terminalSafePathLiteral(path))
		}
		m.entries = append(m.entries, ChatEntry{
			Kind: "error",
			Content: "The request names an external mutation, but temporary write authority could not be safely established for " +
				strings.Join(paths, ", ") + ". Name an existing exact file or repository directory. Nothing was sent, and shell fallback is not allowed.",
		})
		m.invalidateEntryCache()
		m.recalcViewportHeight()
		m.refreshTranscript()
		m.gotoBottomIfFollowing()
		return nil
	}
	if len(msg.Grants) == 0 && len(msg.WriteGrants) == 0 {
		m.input.Focus()
		return m.submitPreparedInput(msg.Draft)
	}
	grants := append([]agent.ReadGrant(nil), msg.Grants...)
	writeGrants := append([]agent.WriteGrant(nil), msg.WriteGrants...)
	if msg.Authority == ModeAuto {
		return m.beginPathGrantMutation(grants, writeGrants, msg.Draft, true, "add-auto-intents")
	}
	canonical := ""
	kind := agent.ReadGrantDirectory
	if len(grants) > 0 {
		canonical = grants[0].Path
		kind = grants[0].Kind
	} else if len(writeGrants) > 0 {
		canonical = writeGrants[0].Path
		kind = agent.ReadGrantKind(writeGrants[0].Kind)
	}
	m.readScopePrompt = &ReadScopePrompt{
		Canonical:   canonical,
		Workspace:   m.agent.WorkDir(),
		Draft:       msg.Draft,
		Kind:        kind,
		Grants:      grants,
		WriteGrants: writeGrants,
		AutoResume:  true,
	}
	m.input.Blur()
	m.recalcViewportHeight()
	return nil
}

func inspectPromptReadGrants(agentInstance *agent.Agent, candidates []string) []agent.ReadGrant {
	intents := make([]promptPathIntent, 0, len(candidates))
	for _, candidate := range candidates {
		intents = append(intents, promptPathIntent{Literal: candidate})
	}
	grants, _ := inspectPromptReadGrantIntents(agentInstance, intents)
	return grants
}

func inspectPromptReadGrantIntents(agentInstance *agent.Agent, intents []promptPathIntent) ([]agent.ReadGrant, bool) {
	reads, _, _, overflow := inspectPromptPathGrantIntents(agentInstance, intents, false)
	return reads, overflow
}

// inspectPromptPathGrantIntents derives only host-owned capabilities from
// paths the user explicitly typed. Mutation intent never comes from the model:
// it is associated with the nearest user action word during scanning. Every
// writable path is also inspected independently for read access because edit
// workflows commonly need both capabilities.
func inspectPromptPathGrantIntents(agentInstance *agent.Agent, intents []promptPathIntent, allowWrite bool) ([]agent.ReadGrant, []agent.WriteGrant, []string, bool) {
	if agentInstance == nil {
		return nil, nil, nil, false
	}
	type inspectedGroup struct {
		path        string
		candidate   string
		kind        agent.ReadGrantKind
		info        os.FileInfo
		physical    agent.ReadPathInspection
		read        agent.ReadGrant
		hasRead     bool
		denied      bool
		allMutation bool
	}
	type deniedBoundary struct {
		path string
		info os.FileInfo
	}
	groups := make([]inspectedGroup, 0, len(intents))
	groupIndex := make(map[string]int, len(intents))
	deniedBoundaries := make([]deniedBoundary, 0, len(intents))
	unavailable := make([]string, 0, min(len(intents), maxPromptPathIntents))
	seenUnavailable := make(map[string]struct{}, len(intents))
	appendUnavailable := func(path string) {
		if _, duplicate := seenUnavailable[path]; duplicate {
			return
		}
		seenUnavailable[path] = struct{}{}
		unavailable = append(unavailable, path)
	}

	for _, intent := range intents {
		candidate := intent.Literal
		if intent.Denied {
			boundaryCandidate := candidate
			if intent.Fallback != "" {
				boundaryCandidate = intent.Fallback
			}
			if boundaryPath, boundaryInfo, boundaryErr := normalizeDeniedPromptPath(boundaryCandidate); boundaryErr == nil {
				deniedBoundaries = append(deniedBoundaries, deniedBoundary{path: boundaryPath, info: boundaryInfo})
			}
		}
		inspection, err := agentInstance.InspectReadPath(candidate)
		if err != nil && intent.Fallback != "" && errors.Is(err, fs.ErrNotExist) {
			candidate = intent.Fallback
			inspection, err = agentInstance.InspectReadPath(candidate)
		}
		if err != nil {
			inspection.Release()
			if allowWrite && intent.Mutation && !intent.Denied {
				appendUnavailable(candidate)
			}
			continue
		}
		if !inspection.External {
			inspection.Release()
			continue
		}
		if inspection.Kind != agent.ReadGrantExactFile && inspection.Kind != agent.ReadGrantDirectory {
			inspection.Release()
			continue
		}
		key := filepath.Clean(inspection.Path)
		info, _ := os.Stat(inspection.Path)
		index, exists := groupIndex[key]
		if !exists {
			for groupPosition := range groups {
				if groups[groupPosition].physical.SamePhysicalIdentity(inspection) {
					index = groupPosition
					exists = true
					break
				}
			}
		}
		if !exists {
			index = len(groups)
			groupIndex[key] = index
			groups = append(groups, inspectedGroup{
				path: inspection.Path, candidate: candidate, kind: inspection.Kind, info: info, physical: inspection,
				allMutation: true,
			})
		} else {
			groupIndex[key] = index
		}
		group := &groups[index]
		group.denied = group.denied || intent.Denied
		group.allMutation = group.allMutation && intent.Mutation
		if !inspection.AlreadyReadable && !group.hasRead {
			group.read = inspection.Grant()
			group.hasRead = true
		} else {
			inspection.Release()
		}
	}

	readGrants := make([]agent.ReadGrant, 0, min(len(groups), maxPromptPathIntents))
	writeGrants := make([]agent.WriteGrant, 0, min(len(intents), maxPromptPathIntents))
	deniedPaths := append([]deniedBoundary(nil), deniedBoundaries...)
	neutralPaths := make([]deniedBoundary, 0, len(groups))
	for index := range groups {
		if groups[index].denied {
			deniedPaths = append(deniedPaths, deniedBoundary{path: groups[index].path, info: groups[index].info})
		}
		if groups[index].denied || !groups[index].allMutation {
			neutralPaths = append(neutralPaths, deniedBoundary{path: groups[index].path, info: groups[index].info})
		}
	}
	overlapsAny := func(path string, info os.FileInfo, boundaries []deniedBoundary) bool {
		for _, boundary := range boundaries {
			if readScopePathsOverlap(path, boundary.path) ||
				(info != nil && boundary.info != nil && os.SameFile(info, boundary.info)) {
				return true
			}
		}
		return false
	}
	for index := range groups {
		group := &groups[index]
		if group.denied || overlapsAny(group.path, group.info, deniedPaths) {
			if group.hasRead {
				group.read.Release()
			}
			continue
		}
		if group.hasRead {
			readGrants = mergePromptReadGrant(readGrants, group.read)
		}
		if !allowWrite || !group.allMutation || overlapsAny(group.path, group.info, neutralPaths) {
			continue
		}
		inspection, err := agentInstance.InspectWritePath(group.candidate)
		if err != nil {
			inspection.Release()
			appendUnavailable(group.candidate)
			continue
		}
		if filepath.Clean(inspection.Path) != filepath.Clean(group.path) {
			inspection.Release()
			appendUnavailable(group.candidate)
			continue
		}
		if !inspection.External || inspection.AlreadyWritable {
			inspection.Release()
			continue
		}
		if inspection.Kind != agent.WriteGrantExactFile && inspection.Kind != agent.WriteGrantDirectory {
			inspection.Release()
			continue
		}
		writeGrants = mergePromptWriteGrant(writeGrants, inspection.Grant())
	}
	if len(readGrants) > maxPromptPathIntents || len(writeGrants) > maxPromptPathIntents {
		releaseReadGrants(readGrants)
		releaseWriteGrants(writeGrants)
		return nil, nil, nil, true
	}
	return readGrants, writeGrants, unavailable, false
}

// mergePromptReadGrant keeps the smallest explicit authority set. A directory
// covers every exact file or narrower directory below it; an exact file can
// never broaden authority to a parent or sibling.
func mergePromptReadGrant(grants []agent.ReadGrant, candidate agent.ReadGrant) []agent.ReadGrant {
	for _, existing := range grants {
		if existing.Kind == agent.ReadGrantDirectory && readScopePathContains(existing.Path, candidate.Path) {
			candidate.Release()
			return grants
		}
	}
	if candidate.Kind != agent.ReadGrantDirectory {
		return append(grants, candidate)
	}
	kept := grants[:0]
	for _, existing := range grants {
		if readScopePathContains(candidate.Path, existing.Path) {
			existing.Release()
			continue
		}
		kept = append(kept, existing)
	}
	return append(kept, candidate)
}

func mergePromptWriteGrant(grants []agent.WriteGrant, candidate agent.WriteGrant) []agent.WriteGrant {
	for _, existing := range grants {
		if existing.Kind == agent.WriteGrantDirectory && readScopePathContains(existing.Path, candidate.Path) {
			candidate.Release()
			return grants
		}
	}
	if candidate.Kind != agent.WriteGrantDirectory {
		return append(grants, candidate)
	}
	kept := grants[:0]
	for _, existing := range grants {
		if readScopePathContains(candidate.Path, existing.Path) {
			existing.Release()
			continue
		}
		kept = append(kept, existing)
	}
	return append(kept, candidate)
}

func releaseWriteGrants(grants []agent.WriteGrant) {
	for _, grant := range grants {
		grant.Release()
	}
}

func (m *Model) revokeTemporaryWriteScopes() error {
	if m == nil || m.agent == nil || len(m.agent.WriteGrants()) == 0 {
		return nil
	}
	_, err := m.agent.ClearWriteGrants()
	return err
}

func explicitPromptPathCandidates(text string) []string {
	scan := scanExplicitPromptPaths(text)
	result := make([]string, 0, len(scan.Intents))
	for _, intent := range scan.Intents {
		result = append(result, intent.Literal)
	}
	return result
}

type promptPathScan struct {
	Intents        []promptPathIntent
	MoreCandidates bool
}

type promptPathIntent struct {
	Literal  string
	Fallback string
	Mutation bool
	Denied   bool
	Start    int
	End      int
}

func scanExplicitPromptPaths(text string) promptPathScan {
	if strings.TrimSpace(text) == "" {
		return promptPathScan{}
	}
	original := []rune(text)
	remaining := append([]rune(nil), original...)
	quotedSpans := make([][2]int, 0, 8)
	codeSpans := promptPathCodeSpans(original)
	inlineCodePaths := promptPathInlineCodePathTokens(original, codeSpans)
	incompleteDelimiterStart := -1
	for _, span := range codeSpans {
		if promptPathCodeSpanClosed(original, span) {
			continue
		}
		if incompleteDelimiterStart < 0 || span[0] < incompleteDelimiterStart {
			incompleteDelimiterStart = span[0]
		}
	}
	for _, span := range codeSpans {
		for index := max(0, span[0]); index < min(len(remaining), span[1]); index++ {
			remaining[index] = ' '
		}
	}
	result := make([]promptPathIntent, 0, maxPromptPathScanIntents)
	seen := make(map[string]int, maxPromptPathScanIntents)
	occurrences := make(map[string][][2]int, maxPromptPathScanIntents)
	allPathSpans := make([][2]int, 0, maxPromptPathScanIntents)
	moreCandidates := false
	appendCandidate := func(candidate string, exact, trimWrapper bool, start, end int) {
		spanStart := start
		if trimWrapper {
			for _, character := range candidate {
				if !strings.ContainsRune("@([{<", character) {
					break
				}
				spanStart++
			}
		}
		intent := normalizePromptPathIntent(candidate, exact, trimWrapper)
		spanValue := intent.Literal
		if intent.Fallback != "" {
			spanValue = intent.Fallback
		}
		if !looksLikeExplicitHostPath(intent.Literal) && !promptPathBroadBoundaryAlias(spanValue) {
			return
		}
		// Free-token punctuation is only an inspection fallback. Keep sentence
		// separators outside the masked path span so "/a. /b" remains two
		// clauses instead of silently inheriting the first clause's authority.
		spanEnd := min(end, spanStart+len([]rune(spanValue)))
		intent.Start = spanStart
		intent.End = spanEnd
		span := [2]int{spanStart, spanEnd}
		if index, duplicate := seen[intent.Literal]; duplicate {
			if len(allPathSpans) < maxPromptPathScanIntents*2 {
				allPathSpans = append(allPathSpans, span)
				occurrences[intent.Literal] = append(occurrences[intent.Literal], span)
			} else {
				// Repetition is also bounded. Silently dropping a later correction
				// or negation could turn an ambiguous prompt into write authority.
				moreCandidates = true
			}
			// Any exact occurrence wins over a free-token punctuation fallback.
			if intent.Fallback == "" {
				result[index].Fallback = ""
			}
			return
		}
		if len(result) >= maxPromptPathScanIntents {
			moreCandidates = true
			return
		}
		allPathSpans = append(allPathSpans, span)
		occurrences[intent.Literal] = append(occurrences[intent.Literal], span)
		seen[intent.Literal] = len(result)
		result = append(result, intent)
	}
	for _, token := range inlineCodePaths {
		appendCandidate(token.Value, true, false, token.Start, token.End)
	}

	for index := 0; index < len(remaining); index++ {
		quote := remaining[index]
		closingQuote, quoted := promptPathClosingQuote(quote)
		if !quoted {
			continue
		}
		if quote == '\'' && promptPathWordApostrophe(remaining, index) {
			continue
		}
		end := index + 1
		for ; end < len(remaining); end++ {
			if quote == '\'' && promptPathWordApostrophe(remaining, end) {
				continue
			}
			if remaining[end] == closingQuote && (end == index+1 || remaining[end-1] != '\\') {
				break
			}
		}
		if end >= len(remaining) {
			// An unmatched quote is incomplete user data, never authority. Mask
			// through the end just like an unclosed Markdown fence so a quoted
			// mutation verb cannot leak into the host policy parser.
			quotedSpans = append(quotedSpans, [2]int{index, len(remaining)})
			if incompleteDelimiterStart < 0 || index < incompleteDelimiterStart {
				incompleteDelimiterStart = index
			}
			for cursor := index; cursor < len(remaining); cursor++ {
				remaining[cursor] = ' '
			}
			break
		}
		quotedSpans = append(quotedSpans, [2]int{index, end + 1})
		appendCandidate(string(remaining[index+1:end]), true, false, index+1, end)
		for cursor := index; cursor <= end; cursor++ {
			remaining[cursor] = ' '
		}
		index = end
	}

	for _, token := range splitPromptPathTokens(remaining) {
		appendCandidate(token.Value, token.EscapedWhitespace, true, token.Start, token.End)
	}
	sort.SliceStable(result, func(left, right int) bool { return result[left].Start < result[right].Start })
	language := []rune(text)
	// Quoted prose and code are data, not authority. A path may itself be
	// quoted (and remains a candidate above), but verbs inside quotes or
	// fenced examples must never silently grant AUTO mutation authority.
	for _, span := range append(quotedSpans, codeSpans...) {
		for index := max(0, span[0]); index < min(len(language), span[1]); index++ {
			language[index] = ' '
		}
	}
	for _, span := range allPathSpans {
		for index := max(0, span[0]); index < min(len(language), span[1]); index++ {
			language[index] = ' '
		}
	}
	for index := range result {
		allDirectMutations := len(occurrences[result[index].Literal]) > 0
		denied := false
		for _, span := range occurrences[result[index].Literal] {
			if promptPathHasExplicitDenial(language, span[0], span[1]) {
				denied = true
			}
			directMutation := promptPathHasMutationIntent(language, span[0], span[1])
			trailingDenied, trailingMutationVeto := promptPathTrailingAuthorityDecision(
				language, span[1], promptPathTrailingScopeEnd(language, span[1], allPathSpans), directMutation,
			)
			if trailingDenied {
				denied = true
			}
			if !directMutation || trailingMutationVeto || trailingDenied {
				allDirectMutations = false
			}
			if promptPathHasAmbiguousRemovalIntent(language, span[0], span[1], allPathSpans) {
				allDirectMutations = false
			}
		}
		result[index].Mutation = allDirectMutations
		result[index].Denied = denied
		if incompleteDelimiterStart >= 0 && result[index].Start < incompleteDelimiterStart {
			result[index].Mutation = false
			result[index].Denied = true
		}
	}
	// Home/root aliases are useful as negative containment boundaries but are
	// intentionally never positive capabilities. Keep them only when the user
	// explicitly denied access; a bare `~` must not authorize the whole home.
	kept := result[:0]
	for _, intent := range result {
		boundary := intent.Literal
		if intent.Fallback != "" {
			boundary = intent.Fallback
		}
		if promptPathBroadBoundaryAlias(boundary) && !intent.Denied {
			continue
		}
		kept = append(kept, intent)
	}
	return promptPathScan{Intents: kept, MoreCandidates: moreCandidates}
}

func promptPathWordApostrophe(input []rune, index int) bool {
	return index > 0 && index+1 < len(input) &&
		unicode.IsLetter(input[index-1]) && unicode.IsLetter(input[index+1])
}

func promptPathClosingQuote(opener rune) (rune, bool) {
	switch opener {
	case '\'', '"':
		return opener, true
	case '‘':
		return '’', true
	case '“':
		return '”', true
	default:
		return 0, false
	}
}

// promptPathCodeSpans returns Markdown fenced-code regions. An unclosed fence
// consumes the remainder of the prompt: incomplete pasted code still cannot
// become host authority.
func promptPathCodeSpans(input []rune) [][2]int {
	spans := make([][2]int, 0, 4)
	for index := 0; index < len(input); {
		marker := input[index]
		if marker != '`' && marker != '~' {
			index++
			continue
		}
		openerLength := promptPathMarkerRun(input, index, marker)
		if openerLength < 3 {
			// CommonMark inline code permits any matching backtick delimiter
			// length. Mask the whole run rather than letting ``update`` become two
			// empty quoted spans with a live authority verb between them. Tildes
			// shorter than a fence remain ordinary prose.
			if marker != '`' {
				index += openerLength
				continue
			}
			start := index
			index += openerLength
			end := len(input)
			for index < len(input) {
				if input[index] != marker {
					index++
					continue
				}
				closingLength := promptPathMarkerRun(input, index, marker)
				if closingLength == openerLength {
					end = index + closingLength
					index = end
					break
				}
				index += closingLength
			}
			spans = append(spans, [2]int{start, end})
			if end == len(input) {
				break
			}
			continue
		}
		start := index
		index += openerLength
		end := len(input)
		for index+2 < len(input) {
			if input[index] == marker {
				closingLength := promptPathMarkerRun(input, index, marker)
				if closingEnd, closes := promptPathFenceCloser(input, index, marker, openerLength); closes {
					end = closingEnd
					index = end
					break
				}
				index += closingLength
				continue
			}
			index++
		}
		spans = append(spans, [2]int{start, end})
		if end == len(input) {
			break
		}
	}
	return spans
}

// promptPathInlineCodePathTokens preserves explicit path literals such as
// `~/notes/file.md` while the surrounding inline-code span remains masked as
// prose authority. Fenced code and unclosed delimiters never produce paths.
func promptPathInlineCodePathTokens(input []rune, spans [][2]int) []promptPathToken {
	result := make([]promptPathToken, 0, len(spans))
	for _, span := range spans {
		if span[0] < 0 || span[1] > len(input) || span[0] >= span[1] || input[span[0]] != '`' {
			continue
		}
		delimiter := promptPathMarkerRun(input, span[0], '`')
		if delimiter < 1 || delimiter >= 3 || span[1]-span[0] <= delimiter*2 {
			continue
		}
		closingStart := span[1] - delimiter
		closed := true
		for index := closingStart; index < span[1]; index++ {
			if input[index] != '`' {
				closed = false
				break
			}
		}
		if !closed {
			continue
		}
		start, end := span[0]+delimiter, closingStart
		for start < end && unicode.IsSpace(input[start]) {
			start++
		}
		for end > start && unicode.IsSpace(input[end-1]) {
			end--
		}
		if start < end {
			result = append(result, promptPathToken{Value: string(input[start:end]), Start: start, End: end})
		}
	}
	return result
}

func promptPathCodeSpanClosed(input []rune, span [2]int) bool {
	if span[0] < 0 || span[1] > len(input) || span[0] >= span[1] {
		return false
	}
	marker := input[span[0]]
	delimiter := promptPathMarkerRun(input, span[0], marker)
	if delimiter < 3 {
		if marker != '`' || span[1]-span[0] <= delimiter*2 {
			return false
		}
		for index := span[1] - delimiter; index < span[1]; index++ {
			if input[index] != marker {
				return false
			}
		}
		return true
	}
	closingStart := span[1] - 1
	for closingStart >= span[0] && (input[closingStart] == ' ' || input[closingStart] == '\t' || input[closingStart] == '\n' || input[closingStart] == '\r') {
		closingStart--
	}
	for closingStart >= span[0] && input[closingStart] == marker {
		closingStart--
	}
	closingStart++
	closingEnd, closed := promptPathFenceCloser(input, closingStart, marker, delimiter)
	return closed && closingEnd == span[1]
}

func promptPathFenceCloser(input []rune, start int, marker rune, openerLength int) (int, bool) {
	lineStart := start
	for lineStart > 0 && input[lineStart-1] != '\n' && input[lineStart-1] != '\r' {
		lineStart--
	}
	indent := 0
	for cursor := lineStart; cursor < start; cursor++ {
		if input[cursor] != ' ' || indent >= 3 {
			return 0, false
		}
		indent++
	}
	runLength := promptPathMarkerRun(input, start, marker)
	if runLength < openerLength {
		return 0, false
	}
	end := start + runLength
	for end < len(input) && (input[end] == ' ' || input[end] == '\t') {
		end++
	}
	if end < len(input) && input[end] != '\n' && input[end] != '\r' {
		return 0, false
	}
	return end, true
}

func promptPathMarkerRun(input []rune, start int, marker rune) int {
	end := start
	for end < len(input) && input[end] == marker {
		end++
	}
	return end - start
}

func promptPathClauseStart(input []rune, pathStart int) int {
	left := pathStart
	for left > 0 {
		boundary := left - 1
		if promptPathSentenceBoundary(input[boundary]) ||
			((input[boundary] == '\n' || input[boundary] == '\r') && promptPathBlankLineAt(input, boundary)) {
			break
		}
		left--
	}
	return left
}

func promptPathNearestAction(input []rune, pathStart, pathEnd int) ([]promptPathWord, int, promptPathAction, bool) {
	if pathStart < 0 || pathEnd < pathStart || pathEnd > len(input) {
		return nil, -1, promptPathActionNone, false
	}
	words := promptPathWords(input, promptPathClauseStart(input, pathStart), pathStart)
	lineStart := pathStart
	for lineStart > 0 && input[lineStart-1] != '\n' && input[lineStart-1] != '\r' {
		lineStart--
	}
	for index := len(words) - 1; index >= 0; index-- {
		// A bare path or data label on a new line is not another destination for
		// the previous line's write verb. Require an action on the path's own line;
		// this also keeps pasted source:/workspace: inventories read-only.
		if words[index].end <= lineStart {
			break
		}
		action := classifyPromptPathAction(words[index].value)
		if action == promptPathActionNone {
			continue
		}
		if promptPathMeaningfulRuneCount(input, words[index].end, pathStart) > 72 || len(words)-index-1 > 8 {
			break
		}
		return words, index, action, true
	}
	return words, -1, promptPathActionNone, false
}

// promptPathMeaningfulRuneCount bounds policy prose while ignoring explicit
// path bodies that were already masked to spaces. A long first destination in
// "update /very/long/a and /very/long/b" must not consume the authority
// distance budget for the second destination.
func promptPathMeaningfulRuneCount(input []rune, start, end int) int {
	start, end = max(0, start), min(len(input), end)
	if start >= end {
		return 0
	}
	count := 0
	for _, character := range input[start:end] {
		if !unicode.IsSpace(character) {
			count++
		}
	}
	return count
}

func promptPathHasExplicitDenial(input []rune, pathStart, pathEnd int) bool {
	words := promptPathWords(input, promptPathClauseStart(input, pathStart), pathStart)
	values := make([]string, 0, len(words))
	for _, word := range words {
		values = append(values, strings.Trim(word.value, "'’- "))
	}
	joined := " " + strings.Join(values, " ") + " "
	for _, phrase := range []string{
		" without reading ", " without accessing ", " without touching ",
		" keep out of ", " stay out of ", " keep away from ", " stay away from ", " anything except ", " everything except ",
		" don't do anything with ", " dont do anything with ", " do not do anything with ", " hold off on ", " everything but ",
		" sin leer ", " sin acceder ", " sin tocar ", " mantente fuera de ", " cualquier cosa excepto ", " todo excepto ", " todo menos ",
	} {
		if strings.Contains(joined, phrase) {
			return true
		}
	}
	for index := len(words) - 1; index >= 0; index-- {
		if pathStart-words[index].end > 72 || len(words)-index-1 > 8 {
			break
		}
		value := strings.Trim(words[index].value, "'’- ")
		if action := classifyPromptPathAction(value); action != promptPathActionNone && promptPathActionNegated(words, index) {
			return true
		}
		if promptPathDenyOnlyAction(value, words, index) {
			return true
		}
	}
	return false
}

func promptPathDenyOnlyAction(value string, words []promptPathWord, index int) bool {
	switch value {
	case "ignore", "avoid", "exclude", "skip", "leave", "except",
		"ignora", "evita", "excluye", "omite", "salta", "deja", "excepto":
		return !promptPathActionNegated(words, index)
	case "access", "touch", "accede", "acceder", "accedas", "toca", "tocar", "toques":
		return promptPathActionNegated(words, index)
	default:
		return false
	}
}

func promptPathDenyOnlyWord(value string) bool {
	value = strings.Trim(strings.ToLower(value), "'’- ")
	switch value {
	case "ignore", "avoid", "exclude", "skip", "leave", "except", "keep",
		"ignora", "evita", "excluye", "omite", "salta", "deja", "excepto", "mantente":
		return true
	default:
		return false
	}
}

func promptPathHasAmbiguousRemovalIntent(input []rune, pathStart, pathEnd int, spans [][2]int) bool {
	words, index, _, ok := promptPathNearestAction(input, pathStart, pathEnd)
	if !ok || !promptPathRemovalAction(words[index].value) {
		return false
	}
	clauseStart := promptPathClauseStart(input, pathStart)
	count := 0
	for _, span := range spans {
		if promptPathClauseStart(input, span[0]) == clauseStart {
			count++
			if count > 1 {
				return true
			}
		}
	}
	return false
}

func promptPathRemovalAction(action string) bool {
	switch strings.Trim(strings.ToLower(action), "'’- ") {
	case "remove", "delete", "eliminar", "elimina", "elimines", "borrar", "borra":
		return true
	default:
		return false
	}
}

// splitPromptPathTokens recognizes the backslash-escaped whitespace emitted by
// macOS drag-and-drop without invoking a shell or interpreting substitutions.
type promptPathToken struct {
	Value             string
	EscapedWhitespace bool
	Start             int
	End               int
}

func splitPromptPathTokens(input []rune) []promptPathToken {
	result := make([]promptPathToken, 0, maxPromptPathScanIntents)
	var current strings.Builder
	escapedWhitespace := false
	start := -1
	end := -1
	flush := func() {
		if current.Len() == 0 {
			return
		}
		result = append(result, promptPathToken{
			Value: current.String(), EscapedWhitespace: escapedWhitespace, Start: start, End: end,
		})
		current.Reset()
		escapedWhitespace = false
		start = -1
		end = -1
	}
	for index := 0; index < len(input); index++ {
		character := input[index]
		if character == '\\' && index+1 < len(input) && unicode.IsSpace(input[index+1]) {
			if start < 0 {
				start = index
			}
			current.WriteRune(input[index+1])
			escapedWhitespace = true
			index++
			end = index + 1
			continue
		}
		if unicode.IsSpace(character) {
			flush()
			continue
		}
		if start < 0 {
			start = index
		}
		current.WriteRune(character)
		end = index + 1
	}
	flush()
	return result
}

type promptPathAction uint8

const (
	promptPathActionNone promptPathAction = iota
	promptPathActionRead
	promptPathActionWrite
)

type promptPathWord struct {
	value string
	start int
	end   int
}

// promptPathHasMutationIntent accepts only a direct, non-negated user action
// before the path. Exact words are intentional: prefix matching turns nouns
// such as "workspace", "configuration", "additional", and "generated" into
// silent AUTO write grants. The narrow grammar also keeps source/reference
// paths read-only in requests that name both inputs and destinations.
func promptPathHasMutationIntent(input []rune, pathStart, pathEnd int) bool {
	if promptPathHasDataLabel(input, pathStart) {
		return false
	}
	words, bestIndex, bestAction, ok := promptPathNearestAction(input, pathStart, pathEnd)
	if !ok || promptPathActionNegated(words, bestIndex) || bestAction != promptPathActionWrite ||
		!promptPathActionDirectRequest(words, bestIndex) {
		return false
	}
	actionWord := strings.Trim(words[bestIndex].value, "'’- ")
	writeTarget := !promptPathActionTakesSource(actionWord)
	for _, word := range words[bestIndex+1:] {
		wordValue := strings.Trim(word.value, "'’– ")
		if wordValue == "and" || wordValue == "y" {
			// An explicit conjunction can join multiple destinations after the
			// intervening path body has been masked. A nearer action (for example
			// "and review /source") still wins in promptPathNearestAction.
			continue
		}
		if promptPathSourceOrTopicWord(wordValue) ||
			promptPathRemovalAction(actionWord) && promptPathRemovalReferenceWord(wordValue) {
			return false
		}
		switch wordValue {
		case "to", "into", "in", "within", "onto", "at",
			"hacia", "a", "en", "dentro":
			writeTarget = true
		default:
			if !promptPathTargetFillerWord(wordValue) {
				return false
			}
		}
	}
	return writeTarget
}

// promptPathHasDataLabel prevents a same-line inventory field from inheriting
// a preceding write verb. These labels describe inputs or context; they are
// not mutation targets even in compact prompts such as
// "update /destination, source: /reference".
func promptPathHasDataLabel(input []rune, pathStart int) bool {
	if pathStart <= 0 || pathStart > len(input) {
		return false
	}
	cursor := pathStart - 1
	for cursor >= 0 && unicode.IsSpace(input[cursor]) {
		cursor--
	}
	if cursor < 0 || input[cursor] != ':' {
		return false
	}
	cursor--
	for cursor >= 0 && (input[cursor] == ' ' || input[cursor] == '\t') {
		cursor--
	}
	end := cursor + 1
	for cursor >= 0 && (unicode.IsLetter(input[cursor]) || input[cursor] == '-' || input[cursor] == '_') {
		cursor--
	}
	label := strings.ToLower(string(input[cursor+1 : end]))
	switch label {
	case "source", "workspace", "input", "reference", "context", "origin",
		"fuente", "espacio", "entrada", "referencia", "contexto", "origen":
		return true
	default:
		return false
	}
}

func promptPathSourceOrTopicWord(word string) bool {
	switch word {
	case "from", "using", "use", "referencing", "reference", "references", "based", "according", "with", "via", "through", "by",
		"of", "for", "on", "about", "regarding", "around", "against", "after", "before", "while", "then", "and",
		"link", "links", "dependency", "dependencies", "report", "summary", "adapter", "api",
		"reading", "inspecting", "reviewing", "auditing", "comparing", "looking", "investigating", "analyzing", "analysing", "summarizing", "checking", "viewing", "opening", "consulting",
		"desde", "usando", "usa", "basado", "según", "segun", "con", "mediante", "de", "para", "sobre", "acerca", "alrededor", "contra", "tras", "antes", "mientras", "luego", "y", "por",
		"referencia", "referencias", "enlace", "enlaces", "dependencia", "dependencias", "reporte", "resumen", "adaptador":
		return true
	default:
		return false
	}
}

type promptPathTrailingDecision struct {
	denyAll   bool
	vetoWrite bool
}

func promptPathTrailingScopeEnd(input []rune, pathEnd int, spans [][2]int) int {
	// Consecutive paths without a new action share one trailing correction:
	// "update /a and /b, but ask me first" must not grant /a. A new explicit
	// action before a later path starts an independent instruction and bounds
	// the earlier decision.
	cursor := pathEnd
	for {
		next := -1
		nextEnd := -1
		for _, span := range spans {
			if span[0] < cursor || (next >= 0 && span[0] >= next) {
				continue
			}
			next, nextEnd = span[0], span[1]
		}
		if next < 0 {
			return -1
		}
		for _, word := range promptPathWords(input, cursor, next) {
			if classifyPromptPathAction(word.value) != promptPathActionNone || promptPathDenyOnlyWord(word.value) {
				return next
			}
		}
		cursor = max(nextEnd, next+1)
	}
}

// promptPathTrailingAuthorityDecision evaluates corrections after a path, even
// across punctuation such as "Ask me first." It stops before the next explicit
// path so one path's instruction cannot rewrite another's authority. Parsing is
// bounded, and overflow fails closed instead of silently dropping a late veto.
func promptPathTrailingAuthorityDecision(input []rune, pathEnd, nextPathStart int, directMutation bool) (bool, bool) {
	if pathEnd < 0 || pathEnd >= len(input) {
		return false, false
	}
	const (
		maxTrailingRunes = 512
		maxTrailingWords = 64
	)
	logicalEnd := len(input)
	if nextPathStart >= pathEnd && nextPathStart < logicalEnd {
		logicalEnd = nextPathStart
	}
	// Explicit path bodies are already masked to whitespace. Bound meaningful
	// trailing prose rather than byte/rune distance so a list of long absolute
	// paths does not make only its first item fail closed while later siblings
	// receive authority.
	compact := make([]rune, 0, min(maxTrailingRunes, max(0, logicalEnd-pathEnd)))
	pendingSpace := false
	overflow := false
	for index := pathEnd; index < logicalEnd; index++ {
		character := input[index]
		if unicode.IsSpace(character) {
			pendingSpace = len(compact) > 0
			continue
		}
		if pendingSpace {
			compact = append(compact, ' ')
			pendingSpace = false
		}
		if len(compact) >= maxTrailingRunes {
			overflow = true
			break
		}
		compact = append(compact, character)
	}
	words := promptPathWords(compact, 0, len(compact))
	overflow = overflow || len(words) > maxTrailingWords
	if len(words) > maxTrailingWords {
		words = words[:maxTrailingWords]
	}
	decision := promptPathTrailingDecision{}
	values := make([]string, 0, len(words))
	for index, word := range words {
		value := strings.Trim(word.value, "'’- ")
		values = append(values, value)
		switch action := classifyPromptPathAction(value); action {
		case promptPathActionRead:
			if promptPathActionNegated(words, index) {
				decision.denyAll = true
			}
		case promptPathActionWrite:
			if promptPathActionNegated(words, index) {
				decision.vetoWrite = true
			}
		}
		if promptPathDenyOnlyAction(value, words, index) {
			decision.denyAll = true
		}
		switch value {
		case "readonly", "read-only", "unchanged",
			"sin", "cambios":
			decision.vetoWrite = true
		case "wait", "unless", "later",
			"tomorrow", "eventually", "pending",
			"espera", "salvo", "después", "despues", "luego", "mañana", "manana", "eventualmente", "pendiente",
			"apruebe", "autorice":
			decision.denyAll = true
			decision.vetoWrite = true
		}
	}
	joined := " " + strings.Join(values, " ") + " "
	trimmed := strings.TrimSpace(strings.Join(values, " "))
	if trimmed == "failed" || trimmed == "failure" || trimmed == "falló" || trimmed == "fallo" || trimmed == "fallido" {
		decision.vetoWrite = true
	}
	for _, phrase := range []string{
		" no changes ", " don't change ", " dont change ", " do not change ", " don't modify ", " dont modify ", " do not modify ",
		" only read ", " only review ", " only inspect ",
		" sin cambios ", " no cambies ", " no modificar ", " solo lee ", " sólo lee ", " solo revisa ", " sólo revisa ",
	} {
		if strings.Contains(joined, phrase) {
			decision.vetoWrite = true
		}
	}
	for _, phrase := range []string{
		" ask me first ", " confirm with me ", " wait for me ", " wait until ", " only after ", " not until ", " unless i ", " unless you ",
		" only if ", " if i approve ", " if i confirm ", " when approved ", " when authorized ", " when i confirm ",
		" once approved ", " once authorized ", " once i confirm ", " upon approval ", " subject to approval ",
		" not now ", " not yet ", " once i tell you ", " once i say so ", " once i say go ", " when i am ready ",
		" after i approve ", " after approval ", " after confirmation ", " after i confirm ", " after you confirm ",
		" pending approval ", " pending confirmation ", " needs approval ", " needs confirmation ", " requires approval ", " requires confirmation ",
		" when i say so ", " when i tell you ", " after i give you the green light ", " green light ", " pending my okay ", " pending my ok ", " pending my approval ",
		" leave it alone ", " stay away ", " keep away ", " stay out ", " keep out ", " is off limits ", " are off limits ",
		" pregúntame primero ", " preguntame primero ", " confirma conmigo ", " espera hasta ", " solo después ", " sólo después ",
		" cuando yo diga ", " cuando te diga ", " después de que te dé luz verde ", " despues de que te de luz verde ", " luz verde ", " pendiente de mi aprobación ", " pendiente de mi aprobacion ",
		" déjalo en paz ", " dejalo en paz ", " está fuera de límites ", " esta fuera de limites ",
	} {
		if strings.Contains(joined, phrase) {
			decision.denyAll = true
			decision.vetoWrite = true
		}
	}
	for _, phrase := range []string{
		" is the command ", " was the command ", " command shown ", " command displayed ",
		" is an example ", " was an example ", " ejemplo mostrado ", " comando mostrado ",
	} {
		if strings.Contains(joined, phrase) {
			decision.vetoWrite = true
		}
	}
	if !directMutation && decision.vetoWrite {
		// For a read request, delayed consent or a read-only correction is a
		// veto on the only authority being considered.
		decision.denyAll = true
	}
	if overflow {
		decision.denyAll = true
		decision.vetoWrite = true
	}
	return decision.denyAll, decision.vetoWrite
}

func promptPathTargetFillerWord(word string) bool {
	switch word {
	case "a", "an", "the", "this", "that", "our", "my", "your", "its", "file", "folder", "directory", "repo", "repository", "project", "workspace", "path", "config", "configuration", "docs", "documentation", "notes", "code", "tests", "test", "line", "entry", "record", "content", "source", "target", "destination", "package", "tool", "library", "registry",
		"el", "la", "los", "las", "un", "una", "este", "esta", "mi", "mis", "tu", "tus", "nuestro", "nuestra", "archivo", "carpeta", "directorio", "repositorio", "proyecto", "ruta", "configuración", "configuracion", "documentación", "documentacion", "notas", "código", "codigo", "pruebas", "prueba", "línea", "linea", "contenido", "destino":
		return true
	default:
		return false
	}
}

func promptPathRemovalReferenceWord(word string) bool {
	switch word {
	case "reference", "references", "link", "links", "dependency", "dependencies",
		"referencia", "referencias", "enlace", "enlaces", "dependencia", "dependencias":
		return true
	default:
		return false
	}
}

func promptPathActionTakesSource(action string) bool {
	switch action {
	case "copy", "copiar", "copia", "copies",
		"add", "agregar", "agrega", "agregues", "añadir", "añade", "añadas", "anadas",
		"install", "instalar", "instala", "instales",
		"register", "registrar", "registra", "registres",
		"integrate", "integrar", "integra", "integres",
		"wire", "conectar", "conecta", "conectes":
		return true
	default:
		return false
	}
}

func promptPathSentenceBoundary(character rune) bool {
	switch character {
	case '.', '!', '?', ';', '。', '！', '？', '…':
		return true
	default:
		return false
	}
}

func promptPathBlankLineAt(input []rune, index int) bool {
	previous := index - 1
	for previous >= 0 && (input[previous] == ' ' || input[previous] == '\t' || input[previous] == '\r') {
		previous--
	}
	if previous >= 0 && input[previous] == '\n' {
		return true
	}
	next := index + 1
	for next < len(input) && (input[next] == ' ' || input[next] == '\t' || input[next] == '\r') {
		next++
	}
	return next < len(input) && input[next] == '\n'
}

func promptPathWords(input []rune, start, end int) []promptPathWord {
	words := make([]promptPathWord, 0, 16)
	wordStart := -1
	flush := func(position int) {
		if wordStart < 0 {
			return
		}
		words = append(words, promptPathWord{
			value: strings.ToLower(string(input[wordStart:position])), start: wordStart, end: position,
		})
		wordStart = -1
	}
	for index := start; index < end; index++ {
		character := input[index]
		if unicode.IsLetter(character) || character == '\'' || character == '’' || character == '-' {
			if wordStart < 0 {
				wordStart = index
			}
			continue
		}
		flush(index)
	}
	flush(end)
	return words
}

func promptPathActionNegated(words []promptPathWord, index int) bool {
	if index <= 0 {
		return false
	}
	start := max(0, index-6)
	for position := start; position < index; position++ {
		switch strings.Trim(words[position].value, "'’- ") {
		case "no", "not", "never", "dont", "don't", "sin", "nunca", "jamás", "jamas":
			return true
		}
	}
	return false
}

func promptPathActionDirectRequest(words []promptPathWord, actionIndex int) bool {
	if actionIndex == 0 {
		return true
	}
	prefix := make([]string, 0, actionIndex)
	for _, word := range words[:actionIndex] {
		prefix = append(prefix, strings.Trim(word.value, "'’- "))
	}
	for len(prefix) > 0 {
		switch prefix[0] {
		case "please", "kindly", "also", "now", "next", "por", "favor", "también", "tambien", "ahora":
			prefix = prefix[1:]
		default:
			goto normalized
		}
	}

normalized:
	if len(prefix) == 0 {
		return true
	}
	joined := strings.Join(prefix, " ")
	switch joined {
	case "can you", "could you", "would you", "will you",
		"i want you to", "i need you to", "we need to", "need to", "you can", "go ahead and",
		"without asking", "without asking me",
		"puedes", "podrías", "podrias", "puedes tú", "puedes tu", "podrías tú", "podrias tu",
		"quiero que", "quiero que tú", "quiero que tu", "necesito que", "necesito que tú", "necesito que tu",
		"sin preguntar", "sin preguntarme":
		return true
	}
	last := prefix[len(prefix)-1]
	if last != "and" && last != "then" && last != "y" && last != "luego" && last != "después" && last != "despues" {
		return false
	}
	// A conjunction may start a second explicit action only when the preceding
	// clause was itself a direct, non-negated request. Merely quoting a plan or
	// README that contains an action word is not user authority.
	preceding := words[:actionIndex-1]
	for index := len(preceding) - 1; index >= 0; index-- {
		if classifyPromptPathAction(preceding[index].value) == promptPathActionNone {
			continue
		}
		return !promptPathActionNegated(preceding, index) && promptPathActionDirectRequest(preceding, index)
	}
	return false
}

func classifyPromptPathAction(value string) promptPathAction {
	value = strings.Trim(strings.ToLower(value), "'’- ")
	switch value {
	case "read", "inspect", "review", "audit", "compare", "look", "investigate", "analyze", "analyse", "summarize", "check", "view", "open", "consult", "use",
		"reading", "inspecting", "reviewing", "auditing", "comparing", "looking", "investigating", "analyzing", "analysing", "summarizing", "checking", "viewing", "opening", "consulting", "using",
		"leer", "lee", "inspecciona", "revisa", "audita", "compara", "mira", "investiga", "analiza", "resume",
		"leas", "inspecciones", "revises", "audites", "compares", "mires", "investigues", "analices", "resumas", "consulta", "usa":
		return promptPathActionRead
	case "edit", "update", "change", "modify", "write", "create", "add", "remove", "delete", "fix", "implement",
		"install", "register", "configure", "migrate", "move", "rename", "copy", "generate", "scaffold", "integrate", "wire", "setup", "build", "work",
		"editar", "edita", "actualizar", "actualiza", "cambiar", "cambia", "modificar", "modifica", "escribir", "escribe",
		"crear", "crea", "agregar", "agrega", "añadir", "añade", "eliminar", "elimina", "borrar", "borra", "corregir", "corrige",
		"implementar", "implementa", "instalar", "instala", "registrar", "registra", "configurar", "configura", "migrar", "migra",
		"mover", "mueve", "renombrar", "renombra", "copiar", "copia", "generar", "genera", "integrar", "integra", "conectar", "conecta", "trabajar", "trabaja",
		"actualices", "edites", "modifiques", "escribas", "crees", "agregues", "añadas", "anadas", "elimines", "corrijas",
		"implementes", "instales", "registres", "configures", "migres", "muevas", "renombres", "copies", "generes", "integres", "conectes", "trabajes":
		return promptPathActionWrite
	default:
		return promptPathActionNone
	}
}

func normalizePromptPathIntent(value string, exact, trimWrapper bool) promptPathIntent {
	if trimWrapper {
		value = strings.TrimLeft(value, "@([{<")
	}
	intent := promptPathIntent{Literal: value}
	if exact {
		return intent
	}
	fallback := strings.TrimRight(value, ".,;:!?。！？…)]}>")
	if fallback != value && (looksLikeExplicitHostPath(fallback) || promptPathBroadBoundaryAlias(fallback)) {
		intent.Fallback = fallback
	}
	return intent
}

func promptPathBroadBoundaryAlias(value string) bool {
	value = strings.TrimSpace(value)
	if value == "~" {
		return true
	}
	return strings.HasPrefix(value, "//") && strings.Trim(value, "/") == ""
}

// normalizeDeniedPromptPath retains explicit negative boundaries even when the
// path is intentionally ineligible for a grant (notably "/" and the home
// directory). A denied broad boundary must still suppress narrower candidates.
func normalizeDeniedPromptPath(value string) (string, os.FileInfo, error) {
	value = strings.TrimSpace(value)
	if value == "~" || strings.HasPrefix(value, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", nil, err
		}
		if value == "~" {
			value = home
		} else {
			value = filepath.Join(home, strings.TrimPrefix(value, "~/"))
		}
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", nil, err
	}
	absolute = filepath.Clean(absolute)
	if resolved, resolveErr := filepath.EvalSymlinks(absolute); resolveErr == nil {
		absolute = filepath.Clean(resolved)
	}
	info, _ := os.Stat(absolute)
	return absolute, info, nil
}

func looksLikeExplicitHostPath(value string) bool {
	if value == "" || value == "~" || strings.HasPrefix(value, "//") {
		return false
	}
	if strings.HasPrefix(value, "~/") || strings.HasPrefix(value, `~\`) {
		return true
	}
	return filepath.IsAbs(value)
}

func readScopePathContains(parent, candidate string) bool {
	relative, err := filepath.Rel(parent, candidate)
	if err != nil {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}
