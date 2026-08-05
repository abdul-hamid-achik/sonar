package agent

import (
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/abdul-hamid-achik/sonar/internal/config"
	permissionpkg "github.com/abdul-hamid-achik/sonar/internal/permission"
)

var autoCommandAssignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=[^=]*$`)
var autoCommandAssignmentValue = regexp.MustCompile(`^[A-Za-z0-9_.-]*$`)
var autoSedPrintProgram = regexp.MustCompile(`^(?:[0-9]+|\$)(?:,(?:[0-9]+|\$))?[pP]$`)

const (
	maxAutoCommandBytes    = 16 * 1024
	maxAutoCommandSegments = 16
	maxAutoCommandWords    = 256
)

type autoCommandDisposition uint8

const (
	autoCommandRequiresApproval autoCommandDisposition = iota
	autoCommandAdmitted
)

type autoCommandEffect uint8

const (
	autoCommandEffectNone autoCommandEffect = iota
	autoCommandEffectReadOnly
	autoCommandEffectWorkspaceMutation
	autoCommandEffectWorkspaceExecution
)

type autoCommandReason uint8

const (
	autoCommandReasonAllowed autoCommandReason = iota
	autoCommandReasonEmpty
	autoCommandReasonBounds
	autoCommandReasonDynamicSyntax
	autoCommandReasonAmbiguousComposition
	autoCommandReasonExecutable
	// autoCommandReasonExecutableUncatalogued distinguishes "installed, but
	// the catalog has no argument contract for it" from
	// autoCommandReasonExecutable's "did not resolve to a trusted host path at
	// all". The distinction earns its keep in the refusal text: an installed
	// xcrun/node used to fall through to "arguments outside the host catalog",
	// and the audited session shows the model taking that literally —
	// re-sending the same executable with shuffled arguments and collecting a
	// prompt each time, when no argument form could ever be admitted.
	autoCommandReasonExecutableUncatalogued
	autoCommandReasonArguments
	autoCommandReasonPathAuthority
	// autoCommandReasonHostToolAvailable is a refusal with a remedy: the host
	// has a built-in that does this job under the workspace ignore policy.
	// Reported separately from autoCommandReasonArguments because "arguments
	// outside the host catalog" tells the model nothing it can act on, so it
	// re-sends the same shell command and collects another approval prompt.
	autoCommandReasonHostToolAvailable
	// autoCommandReasonRedirectTarget follows the same precedent: > and >> are
	// admitted when the target resolves inside the workspace, so the remedy is
	// to redirect into a workspace file, and the reason says so instead of
	// failing the whole command as "outside the bounded shell subset".
	autoCommandReasonRedirectTarget
)

// autoCommandAssessment is the bounded host-owned projection of one AUTO
// shell admission decision. It deliberately retains neither the raw command
// nor its arguments: those may contain private values and already live on the
// execution request. The typed effect and provenance flags keep the authority
// boundary inspectable without turning arbitrary shell text into policy.
type autoCommandAssessment struct {
	disposition autoCommandDisposition
	effect      autoCommandEffect
	reason      autoCommandReason
	segments    int
	// refusedSegment indexes the static split segment whose catalog verdict
	// refused the command, or -1 when the refusal was not a per-segment one
	// (empty, bounds, dynamic syntax, composition, redirect, cd path). It is an
	// index rather than the segment's words on purpose: an index cannot carry a
	// private value into an approval surface or the durable ledger, and every
	// caller that needs the words already holds the command text they came from.
	refusedSegment      int
	usesReadGrant       bool
	workspaceExecutable bool
}

func (assessment autoCommandAssessment) admitted() bool {
	return assessment.disposition == autoCommandAdmitted
}

// autoCommandReasonLabel renders one AUTO admission denial as operator-facing
// text. It is a bounded host projection: no raw command text, arguments, or
// filesystem operands flow through it, so private values cannot leak into an
// approval surface or the durable ledger. The command is consulted only to
// name the exact expansion token a DynamicSyntax rejection tripped on.
func autoCommandReasonLabel(reason autoCommandReason, command string) string {
	switch reason {
	case autoCommandReasonEmpty:
		return "empty command"
	case autoCommandReasonBounds:
		return "command exceeds the bounded shell subset"
	case autoCommandReasonDynamicSyntax:
		if token, ok := firstDynamicShellSyntaxToken(command); ok {
			return "dynamic shell syntax (" + token + ")"
		}
		return "dynamic shell syntax"
	case autoCommandReasonAmbiguousComposition:
		return "ambiguous command composition"
	case autoCommandReasonExecutable:
		return "executable outside the host catalog"
	case autoCommandReasonExecutableUncatalogued:
		return "executable installed but outside the host catalog; changing arguments cannot admit it"
	case autoCommandReasonArguments:
		return "arguments outside the host catalog"
	case autoCommandReasonHostToolAvailable:
		return "raw recursive search bypasses the workspace ignore policy; use the grep, glob, ls or read tools instead"
	case autoCommandReasonRedirectTarget:
		return "redirect only into workspace files; this redirect target resolves outside the workspace"
	case autoCommandReasonPathAuthority:
		return "operand outside the workspace"
	case autoCommandReasonAllowed:
		return "admitted by the scoped shell policy"
	default:
		return "outside the scoped shell policy"
	}
}

// firstDynamicShellSyntaxToken mirrors hasDynamicShellSyntax and reports the
// first expansion, grouping, or substitution token the bounded scanner
// refuses, so the operator sees exactly what tripped the rule instead of a
// bare rejection.
func firstDynamicShellSyntaxToken(command string) (string, bool) {
	runes := []rune(command)
	var quote rune
	escaped := false
	for index := 0; index < len(runes); index++ {
		character := runes[index]
		if escaped {
			escaped = false
			continue
		}
		if character == '\\' && quote != '\'' {
			escaped = true
			continue
		}
		if quote == '\'' {
			if character == quote {
				quote = 0
			}
			continue
		}
		if quote == '"' {
			switch character {
			case '"':
				quote = 0
			case '$':
				// An inert parameter is admitted, so naming it here would
				// blame `$?` for a refusal actually caused by a `$(…)` later
				// in the same command.
				if inertShellParameter(runes, index) {
					index++
					continue
				}
				return dynamicShellToken(runes, index), true
			case '`':
				return dynamicShellToken(runes, index), true
			}
			continue
		}
		switch character {
		case '\'', '"':
			quote = character
		case '$':
			if inertShellParameter(runes, index) {
				index++
				continue
			}
			return dynamicShellToken(runes, index), true
		case '`', '(', ')', '{', '}':
			return dynamicShellToken(runes, index), true
		}
	}
	return "", false
}

// dynamicShellToken names the offending expansion. The common `$?`, `$[`, and
// `$#` forms stay visible as a pair; every other trigger reports its single
// rune (so `$(` reads as `$` rather than an unbalanced pair).
func dynamicShellToken(runes []rune, index int) string {
	if runes[index] == '$' && index+1 < len(runes) {
		switch runes[index+1] {
		case '?', '[', '#', '*', '@', '!':
			return string(runes[index : index+2])
		}
	}
	return string(runes[index])
}

// autoCommandApprovalReason renders the host-owned reason a bash request was
// not admitted by the scoped AUTO shell policy, or "" when that policy is not
// in effect or the request would have been admitted. Callers pass the turn's
// captured authority mode so a mid-turn UI mode change cannot relabel a prompt.
func (a *Agent) autoCommandApprovalReason(mode AuthorityMode, command string) string {
	if mode != AuthorityAutoScoped {
		return ""
	}
	assessment := a.assessAutoScopedCommand(command)
	if assessment.admitted() {
		return ""
	}
	return autoCommandReasonLabel(assessment.reason, command)
}

// autoCommandGrantPrefix names the bash prefix a saved grant must carry to
// cure this refusal — the one derived from the segment the host refused.
//
// The host is the only party that knows which segment that was, and the
// difference is not academic. Every prompt in the audited session 8c7ca7f read
// `cd … && sed -n … && echo … && grep …`: the grep segment refused, whole-
// command derivation offered "sed" (cd and echo are skipped as trivial, sed is
// simply first), and the ledger shows the consequence — always pressed at
// iterations 10 and 12, iterations 11, 14, 16 and 17 prompting again with the
// identical reason. Three grants, none of which could ever match the segment
// that would be re-assessed.
//
// It returns "" rather than guessing whenever the refusal is not curable by a
// prefix at all (composition, dynamic syntax, path and redirect refusals are
// re-checked identically under a grant), leaving callers on the whole-command
// derivation. Narrowing an offer is the whole job here; widening one is not.
func (a *Agent) autoCommandGrantPrefix(mode AuthorityMode, command string) string {
	if mode != AuthorityAutoScoped {
		return ""
	}
	assessment := a.assessAutoScopedCommand(command)
	if assessment.admitted() || assessment.refusedSegment < 0 ||
		!autoCommandGrantEligibleReason(assessment.reason) {
		return ""
	}
	commands, _, _, ok := splitStaticShellCommands(command)
	if !ok || assessment.refusedSegment >= len(commands) {
		return ""
	}
	words := commands[assessment.refusedSegment]
	// Strip leading assignments exactly as the admission and grant paths do, so
	// the offered prefix lines up with the executable those matchers compare.
	for len(words) > 0 && autoCommandAssignmentAllowed(words[0]) {
		words = words[1:]
	}
	prefix, ok := permissionpkg.DeriveBashPrefixFromSegment(words)
	if !ok {
		return ""
	}
	return prefix
}

type autoSimpleCommandAssessment struct {
	allowed             bool
	effect              autoCommandEffect
	reason              autoCommandReason
	usesReadGrant       bool
	workspaceExecutable bool
}

type autoPathAuthority uint8

const (
	autoPathDenied autoPathAuthority = iota
	autoPathWorkspace
	autoPathReadGrant
)

// autoScopedCommandAllowed recognizes a deliberately bounded shell subset for
// AUTO. It is not a shell sandbox: the subprocess still runs under the host's
// ordinary account. The catalog exists to keep routine local build, test,
// formatting, and inspection work flowing while Git, dynamic expansion,
// destructive commands, network-facing CLIs, redirection to files, and
// workspace escapes continue through interactive approval.
func (a *Agent) autoScopedCommandAllowed(command string) bool {
	return a.assessAutoScopedCommand(command).admitted()
}

func (a *Agent) assessAutoScopedCommand(command string) autoCommandAssessment {
	assessment := autoCommandAssessment{disposition: autoCommandRequiresApproval, refusedSegment: -1}
	// The policy scanner and the POSIX shell must observe the same character
	// stream. Invalid UTF-8 can otherwise be normalized differently by rune
	// iteration and by the child process, turning a rejected token boundary into
	// an executable one.
	if !utf8.ValidString(command) {
		assessment.reason = autoCommandReasonDynamicSyntax
		return assessment
	}
	if strings.TrimSpace(command) == "" {
		assessment.reason = autoCommandReasonEmpty
		return assessment
	}
	if len(command) > maxAutoCommandBytes {
		assessment.reason = autoCommandReasonBounds
		return assessment
	}
	if strings.ContainsRune(command, '\r') ||
		hasShellLineContinuation(command) || hasDynamicShellSyntax(command) || hasUnquotedShellGlob(command) {
		assessment.reason = autoCommandReasonDynamicSyntax
		return assessment
	}
	commands, separators, redirects, ok := splitStaticShellCommands(command)
	if !ok || len(commands) == 0 || len(commands) > maxAutoCommandSegments {
		assessment.reason = autoCommandReasonBounds
		return assessment
	}
	assessment.segments = len(commands)
	for index, words := range commands {
		if len(words)+len(redirects[index]) > maxAutoCommandWords {
			assessment.reason = autoCommandReasonBounds
			return assessment
		}
	}
	if len(commands) > 1 && staticCommandsContainExecutable(commands, "cd") {
		// A failed cd must not fall through to a later command. A pipeline after
		// an &&-guarded command is safe (`cd x && query | bounded-filter`), but a
		// cd that is itself a pipeline member runs in an isolated subshell and
		// cannot establish the base directory modeled below.
		for index, separator := range separators {
			if separator == ";" || separator == "||" || separator == "|" &&
				(staticCommandExecutable(commands[index]) == "cd" || staticCommandExecutable(commands[index+1]) == "cd") {
				assessment.reason = autoCommandReasonAmbiguousComposition
				return assessment
			}
		}
	}
	baseDir := a.activeWorkDir()
	plannedMinervaBuild := false
	minervaPrefixEligible := true
	minervaOutputPipeline := false
	for index, words := range commands {
		simple := a.assessAutoScopedSimpleCommand(words, baseDir)
		isMinervaOutputFilter := false
		if minervaOutputPipeline {
			if index == 0 || separators[index-1] != "|" || !a.autoScopedMinervaOutputFilterAllowed(words) {
				assessment.reason = autoCommandReasonAmbiguousComposition
				return assessment
			}
			simple = autoSimpleCommandAssessment{
				allowed: true, effect: autoCommandEffectReadOnly, reason: autoCommandReasonAllowed,
			}
			isMinervaOutputFilter = true
		} else if !simple.allowed && plannedMinervaBuild && index > 0 && separators[index-1] == "&&" &&
			a.autoScopedPlannedMinervaWorkspaceCommandAllowed(words, baseDir) {
			simple = autoSimpleCommandAssessment{
				allowed: true, effect: autoCommandEffectWorkspaceExecution,
				reason: autoCommandReasonAllowed, workspaceExecutable: true,
			}
		}
		// A user-saved bash prefix (an "always" session grant or a durable
		// workspace rule) can supply the executable authority a segment lacks.
		// This is what lets a grant reach INSIDE a compound command: the whole
		// command's composition — splitting, dynamic syntax, redirect targets,
		// path operands — has been or is still validated by the host either
		// way, and the grant cures only catalog refusals, never composition or
		// path ones. Before this, 33 of 34 prompted commands in one audited
		// session carried a composition marker, so a saved prefix could match
		// nothing the model actually sent and every "always" was a placebo.
		if !simple.allowed && autoCommandGrantEligibleReason(simple.reason) {
			if granted, ok := a.grantAuthorizedSegmentAssessment(words, baseDir); ok {
				simple = granted
			}
		}
		if !simple.allowed {
			assessment.reason = simple.reason
			// Name the segment that objected, after the grant re-assessment
			// above has already failed to cure it. This is the only refusal in
			// this function a saved bash prefix can ever reach, so it is the
			// only one worth offering a prefix for.
			assessment.refusedSegment = index
			return assessment
		}
		if simple.workspaceExecutable {
			// Minerva is a bounded query surface, not a generic pipeline source or
			// filter. Accept only an optional leading cd or an immediately preceding
			// exact Minerva build producer. No already-trusted binary may be replaced
			// by a different command earlier in the same shell request.
			for _, separator := range separators[:index] {
				if separator != "&&" {
					assessment.reason = autoCommandReasonAmbiguousComposition
					return assessment
				}
			}
			if !minervaPrefixEligible && !plannedMinervaBuild ||
				index > 0 && separators[index-1] != "&&" {
				assessment.reason = autoCommandReasonAmbiguousComposition
				return assessment
			}
			if index != len(commands)-1 {
				if separators[index] != "|" {
					assessment.reason = autoCommandReasonAmbiguousComposition
					return assessment
				}
				minervaOutputPipeline = true
			}
		} else if isMinervaOutputFilter {
			if index != len(commands)-1 && separators[index] != "|" {
				assessment.reason = autoCommandReasonAmbiguousComposition
				return assessment
			}
		}
		if simple.effect > assessment.effect {
			assessment.effect = simple.effect
		}
		for _, target := range redirects[index] {
			// A redirect target gets the same containment the auto-approved
			// write builtin applies to its path argument: resolved against the
			// segment's effective directory, with ~, parent traversal, and
			// symlink escapes all refused. That last one is why this cannot be
			// a lexical check — /tmp is world-writable, and a pre-planted
			// symlink under the workspace (or an external target outright)
			// would let > truncate an arbitrary file. Temporary external write
			// grants are typed host capabilities and, like read grants above,
			// deliberately never widen raw-shell authority.
			if target == "" || a.autoCommandCandidatePathAssessment(target, baseDir, false) != autoPathWorkspace {
				assessment.reason = autoCommandReasonRedirectTarget
				return assessment
			}
			// `> /dev/null` in its spaced spelling discards output exactly like
			// the glued token the scanner admits; only a real file is a mutation.
			if target != "/dev/null" && assessment.effect < autoCommandEffectWorkspaceMutation {
				assessment.effect = autoCommandEffectWorkspaceMutation
			}
		}
		assessment.usesReadGrant = assessment.usesReadGrant || simple.usesReadGrant
		assessment.workspaceExecutable = assessment.workspaceExecutable || simple.workspaceExecutable
		isMinervaBuild := a.autoScopedMinervaBuildCommandAllowed(words, baseDir)
		isCD := staticCommandExecutable(words) == "cd"
		if !isCD && !isMinervaBuild && !simple.workspaceExecutable && !isMinervaOutputFilter {
			minervaPrefixEligible = false
		}
		if isCD && plannedMinervaBuild {
			minervaPrefixEligible = false
		}
		plannedMinervaBuild = isMinervaBuild
		if isCD {
			var cdOK bool
			baseDir, cdOK = a.autoScopedCDTarget(words, baseDir)
			if !cdOK {
				assessment.reason = autoCommandReasonPathAuthority
				return assessment
			}
		}
	}
	assessment.disposition = autoCommandAdmitted
	assessment.reason = autoCommandReasonAllowed
	return assessment
}

func hasShellLineContinuation(command string) bool {
	// POSIX shells remove backslash-newline before tokenization. The bounded
	// scanner must never authorize a different token stream than sh executes.
	return strings.Contains(command, "\\\n") || strings.Contains(command, "\\\r")
}

// inertShellParameter reports whether the `$` at index introduces a special
// parameter whose expansion is provably a decimal integer.
//
// POSIX fixes the value of each of these: `$?` is the previous exit status,
// `$$` and `$!` are process IDs, `$#` is a positional-parameter count. None can
// expand to a command, a path, or a program name, which is the property the
// dynamic-syntax rule exists to prevent. Everything else after `$` — `(`, `{`,
// a name, a digit — stays refused, so command substitution and variable reads
// are untouched.
//
// This is the measured case, not a guess. In one AUTO session half the approval
// prompts were dynamic-syntax refusals, and the commands behind them were
// ordinary verification runs like `go build ./... 2>&1 | head -30; echo "exit:
// $?"`. The other half were `$(…)` and remain refused.
func inertShellParameter(runes []rune, index int) bool {
	if index+1 >= len(runes) {
		return false
	}
	switch runes[index+1] {
	case '?', '$', '#', '!':
		return true
	default:
		return false
	}
}

func hasDynamicShellSyntax(command string) bool {
	runes := []rune(command)
	var quote rune
	escaped := false
	for index := 0; index < len(runes); index++ {
		character := runes[index]
		if escaped {
			escaped = false
			continue
		}
		if character == '\\' && quote != '\'' {
			escaped = true
			continue
		}
		if quote == '\'' {
			if character == quote {
				quote = 0
			}
			continue
		}
		if quote == '"' {
			switch character {
			case '"':
				quote = 0
			case '$':
				if inertShellParameter(runes, index) {
					// Consume the parameter so its second rune cannot be
					// rescanned as an opener: `$$` must read as one PID, not
					// as a `$` introducing whatever follows.
					index++
					continue
				}
				return true
			case '`':
				return true
			}
			continue
		}
		switch character {
		case '\'', '"':
			quote = character
		case '$':
			if inertShellParameter(runes, index) {
				index++
				continue
			}
			return true
		case '`', '(', ')', '{', '}':
			return true
		}
	}
	return false
}

func hasUnquotedShellGlob(command string) bool {
	var quote rune
	escaped := false
	for _, character := range command {
		if escaped {
			escaped = false
			continue
		}
		if character == '\\' && quote != '\'' {
			escaped = true
			continue
		}
		if quote != 0 {
			if character == quote {
				quote = 0
			}
			continue
		}
		switch character {
		case '\'', '"':
			quote = character
		case '*', '?', '[':
			return true
		}
	}
	return false
}

// splitStaticShellCommands accepts quoted words and foreground &&, ||, ; and
// pipe composition. Expansion, grouping, backgrounding, and input redirection
// are rejected before this point or by the scanner. This keeps command-name
// checks meaningful even when several routine development commands are joined.
//
// A bare > or >> token at a word boundary is parsed rather than refused: its
// target word is collected into the per-segment redirects slice so the
// assessment can hold it to the same workspace containment the auto-approved
// write builtin uses. Before this, AUTO wrote any workspace file through the
// write tool with no prompt while refusing `swift test > build.log` — the same
// effect, spelled as a shell redirect. Every other unquoted < or > (mid-word
// forms such as `2>file`, and all input redirection) still fails the split.
func splitStaticShellCommands(command string) ([][]string, []string, [][]string, bool) {
	trimmed := strings.TrimSpace(command)
	if strings.HasSuffix(trimmed, "&&") || strings.HasSuffix(trimmed, "||") ||
		strings.HasSuffix(trimmed, "|") || strings.HasSuffix(trimmed, "&") {
		return nil, nil, nil, false
	}
	var (
		commands         [][]string
		separators       []string
		redirects        [][]string
		segmentRedirects []string
		words            []string
		word             strings.Builder
		quote            rune
		escaped          bool
		wordOpen         bool
		redirectPending  bool
	)
	flushWord := func() {
		if wordOpen {
			if redirectPending {
				segmentRedirects = append(segmentRedirects, word.String())
				redirectPending = false
			} else {
				words = append(words, word.String())
			}
			word.Reset()
			wordOpen = false
		}
	}
	flushCommand := func() bool {
		flushWord()
		// A separator while a redirect still awaits its target (`echo x > | y`)
		// and a segment that is only a redirect (`x && > f`) both fail closed.
		if redirectPending || len(words) == 0 {
			return false
		}
		commands = append(commands, words)
		redirects = append(redirects, segmentRedirects)
		words = nil
		segmentRedirects = nil
		return true
	}
	runes := []rune(command)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if escaped {
			word.WriteRune(r)
			wordOpen = true
			escaped = false
			continue
		}
		if r == '\\' && quote != '\'' {
			// Within double quotes, POSIX sh only consumes backslash before
			// $, `, ", and backslash (line continuations were rejected above).
			// Preserve it before every other rune so validation sees the same
			// filesystem token the shell will use.
			if quote == '"' && i+1 < len(runes) && !strings.ContainsRune("$`\"\\", runes[i+1]) {
				word.WriteRune(r)
				wordOpen = true
				continue
			}
			escaped = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				word.WriteRune(r)
			}
			continue
		}
		if !wordOpen {
			if redirectLength := staticDescriptorRedirectLength(runes, i); redirectLength > 0 {
				i += redirectLength - 1
				continue
			}
		}
		switch r {
		case '\'', '"':
			quote = r
			wordOpen = true
		case ' ', '\t', '\r':
			flushWord()
		case '\n', ';', '|', '&':
			if r == '&' && (i+1 >= len(runes) || runes[i+1] != '&') {
				return nil, nil, nil, false
			}
			separator := string(r)
			if r == '|' && i+1 < len(runes) && runes[i+1] == '|' {
				i++
				separator = "||"
			} else if r == '&' {
				i++
				separator = "&&"
			} else if r == '\n' {
				separator = ";"
			}
			if !flushCommand() {
				return nil, nil, nil, false
			}
			separators = append(separators, separator)
		case '<':
			return nil, nil, nil, false
		case '>':
			// Only a bare > or >> that begins its own token opens a validated
			// output redirect. Mid-word (`foo2>bar`) and doubled-pending forms
			// stay refused: the shell would parse them as redirects too, but
			// modeling descriptor-numbered targets is not worth the surface.
			if wordOpen || redirectPending {
				return nil, nil, nil, false
			}
			if i+1 < len(runes) && runes[i+1] == '>' {
				i++
			}
			redirectPending = true
		default:
			if unicode.IsControl(r) {
				return nil, nil, nil, false
			}
			word.WriteRune(r)
			wordOpen = true
		}
	}
	if escaped || quote != 0 {
		return nil, nil, nil, false
	}
	flushWord()
	if redirectPending {
		// Trailing `>` with no target names no file; there is nothing to validate.
		return nil, nil, nil, false
	}
	if len(words) > 0 {
		commands = append(commands, words)
		redirects = append(redirects, segmentRedirects)
	}
	if len(separators) != len(commands)-1 {
		return nil, nil, nil, false
	}
	return commands, separators, redirects, len(commands) > 0
}

// staticDescriptorRedirectLength recognizes the byte-exact redirect tokens
// that provably cannot create or modify a file: the descriptor merges 2>&1 and
// 1>&2, and the /dev/null discards 2>/dev/null, 1>/dev/null, and >/dev/null.
// /dev/null is a fixed kernel sink, so these are output-shaping, not writes.
//
// Each token is matched only at a word boundary. The general scanner rejects
// every other unquoted < or >, which is how `2>/tmp/leak` (a real file write)
// and `foo2>/dev/null` (a redirect glued to the word "foo2") stay refused. The
// glued case matters: before the /dev/null tokens were listed here the scanner
// was already inside the word "2" when it met ">", so `swift test 2>/dev/null`
// — half of one audited session's "bounded shell subset" refusals — prompted
// even though it discards output exactly like the admitted 2>&1.
func staticDescriptorRedirectLength(command []rune, offset int) int {
	for _, redirect := range []string{"2>&1", "1>&2", "2>/dev/null", "1>/dev/null", ">/dev/null"} {
		candidate := []rune(redirect)
		if offset+len(candidate) > len(command) {
			continue
		}
		matched := true
		for index := range candidate {
			if command[offset+index] != candidate[index] {
				matched = false
				break
			}
		}
		if !matched {
			continue
		}
		next := offset + len(candidate)
		if next < len(command) && !unicode.IsSpace(command[next]) && !strings.ContainsRune(";|&\n", command[next]) {
			continue
		}
		return len(candidate)
	}
	return 0
}

func staticCommandExecutable(words []string) string {
	for len(words) > 0 && autoCommandAssignmentAllowed(words[0]) {
		words = words[1:]
	}
	if len(words) == 0 || filepath.Base(words[0]) != words[0] {
		return ""
	}
	return words[0]
}

func staticCommandsContainExecutable(commands [][]string, executable string) bool {
	for _, words := range commands {
		if staticCommandExecutable(words) == executable {
			return true
		}
	}
	return false
}

func (a *Agent) autoScopedCDTarget(words []string, baseDir string) (string, bool) {
	for len(words) > 0 && autoCommandAssignmentAllowed(words[0]) {
		words = words[1:]
	}
	if len(words) == 0 || words[0] != "cd" {
		return "", false
	}
	args := words[1:]
	if len(args) == 2 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) != 1 || rawPathHasParentTraversal(args[0]) {
		return "", false
	}
	target := args[0]
	if target == "-" {
		return "", false
	}
	if !filepath.IsAbs(target) && baseDir != "" {
		target = filepath.Join(baseDir, target)
	}
	// Shell directory changes stay confined to the primary workspace. A
	// temporary write grant is authority for typed host operations, not a way to
	// turn an external directory into an ambient shell working directory.
	resolved, err := a.resolveWorkspacePath(target)
	return resolved, err == nil
}

func (a *Agent) assessAutoScopedSimpleCommand(words []string, baseDir string) autoSimpleCommandAssessment {
	assessment := autoSimpleCommandAssessment{reason: autoCommandReasonExecutable}
	for len(words) > 0 && autoCommandAssignmentAllowed(words[0]) {
		words = words[1:]
	}
	if len(words) == 0 {
		return assessment
	}
	rawExecutable := words[0]
	executable := filepath.Base(words[0])
	if executable == "." || executable == "source" || executable == "eval" || executable == "exec" || executable == "env" {
		return assessment
	}
	// A workspace Minerva binary is the one deliberately narrow exception
	// to the host-path catalog. It is admitted only after physical workspace,
	// Go build-identity, install-location, and exact query-argv validation. This
	// lets AUTO verify the local CLI it just built while Minerva's ordinary
	// product integration continues through its exact trusted MCPHub route.
	if executable != rawExecutable {
		if a.autoScopedMinervaWorkspaceCommandAllowed(rawExecutable, words[1:], baseDir) {
			assessment.allowed = true
			assessment.effect = autoCommandEffectWorkspaceExecution
			assessment.reason = autoCommandReasonAllowed
			assessment.workspaceExecutable = true
		}
		return assessment
	}
	// Every other AUTO shell executable must resolve through the host-owned
	// catalog. A generic path-qualified workspace binary can hide mutation or
	// networking behind an innocent-looking argument and remains approval-gated.
	if !a.autoCommandExecutableAllowed(executable) {
		// The refusal stands either way — this gate is what keeps a
		// workspace-planted binary out of AUTO — but the explanation must not
		// depend on the host's PATH. autoCommandExecutableAllowed resolves
		// through exec.LookPath, so `rg --no-ignore .` explained itself on a
		// machine with ripgrep installed and reported the bare "executable
		// outside the host catalog" on one without. The remedy describes what
		// the model reached for, and it applies at least as strongly when the
		// binary is absent: the built-in grep tool is then the only way to run
		// that search at all.
		if autoCommandHasBuiltinSearchRemedy(executable) {
			assessment.reason = autoCommandReasonHostToolAvailable
		}
		return assessment
	}
	assessment.effect = autoCommandEffectForExecutable(executable, words[1:])
	args := words[1:]
	// Temporary external scopes are typed host capabilities. They intentionally
	// never become ambient raw-shell authority, even for otherwise read-only
	// commands such as cat or sed.
	allowReadGrants := false
	for index, word := range args {
		if autoCommandNonPathArgument(executable, args, index) {
			continue
		}
		authority := a.autoCommandPathAssessment(word, baseDir, allowReadGrants)
		if authority == autoPathDenied {
			assessment.reason = autoCommandReasonPathAuthority
			return assessment
		}
		assessment.usesReadGrant = assessment.usesReadGrant || authority == autoPathReadGrant
	}

	attachedAuthority, attachedOK := a.autoScopedAttachedPathOptionsAssessment(executable, args, baseDir, allowReadGrants)
	if !attachedOK {
		assessment.reason = autoCommandReasonPathAuthority
		return assessment
	}
	assessment.usesReadGrant = assessment.usesReadGrant || attachedAuthority == autoPathReadGrant
	allowed := false
	switch executable {
	case "cd":
		if len(args) == 2 && args[0] == "--" {
			args = args[1:]
		}
		allowed = len(args) == 1 && a.autoCommandCandidatePathAssessment(args[0], baseDir, false) != autoPathDenied
	case "go":
		allowed = autoScopedGoCommandAllowed(args)
	case "git":
		allowed = autoScopedGitCommandAllowed(args)
	case "npm":
		allowed = autoScopedPackageCommandAllowed(args, "test")
	case "pnpm", "yarn":
		allowed = autoScopedPackageCommandAllowed(args, "test", "build", "lint", "check", "typecheck")
	case "bun":
		allowed = autoScopedPackageCommandAllowed(args, "test", "lint")
	case "cargo":
		allowed = autoScopedCargoCommandAllowed(args)
	case "swift":
		allowed = autoScopedSwiftCommandAllowed(args)
	case "sed":
		allowed = autoScopedSedCommandAllowed(args)
	case "grep":
		// Walking is the property that matters, not the executable's name.
		//
		// The bypass this refusal exists to prevent is a search that DISCOVERS
		// descendants: the per-operand loop above resolves every named path
		// through the workspace and the host secret policy, so an operand the
		// ignore policy excludes is already denied — `grep -n SECRET .env` is
		// refused by path authority whether or not this case admits grep. What
		// path authority never sees is a file the command finds for itself, and
		// only a directory walk finds one.
		//
		// Without a recursion option grep reads exactly the operands it was
		// given. A directory among them yields EISDIR on Linux and is skipped on
		// BSD; neither enumerates, so there is no unchecked read either way.
		// That is a structural claim about grep's traversal, not a flag
		// denylist over its behaviour.
		//
		// Refusing the non-recursive form cost real work and explained itself
		// wrongly: seven of the nine approvals in session 8c7ca7f were
		// `grep -n <pattern> <explicit files>`, told they were "raw recursive
		// search" when nothing about them recursed.
		if autoGrepWalksDirectories(args) {
			assessment.reason = autoCommandReasonHostToolAvailable
			return assessment
		}
		allowed = true
	case "find", "rg", "tree", "du", "ls":
		// These enumerate by construction — walking is what they are for, and
		// rg walks the working directory when given no operand at all — so
		// there is no non-walking form to admit. Built-in list/grep/read
		// operations enforce the ignore policy, so they stay approval-gated in
		// AUTO even for workspace operands.
		//
		// The refusal is reported as its own reason because this one has a
		// remedy. In a measured session four of six approval prompts were
		// ordinary searches — `grep -rl "ollama" internal/ui`, `find . -name
		// '*.go'` — that the built-in grep and glob tools serve directly, with
		// the same arguments and without the bypass. Telling the model that is
		// the difference between one prompt and one prompt per attempt.
		assessment.reason = autoCommandReasonHostToolAvailable
		return assessment
	case "sort":
		allowed = !containsLongOptionPrefix(args, "--compress-program", "--files0-from")
	case "printf":
		allowed = autoScopedPrintfCommandAllowed(args)
	case "file":
		allowed = !containsArg(args, "-C", "-S", "-f", "-z", "-Z", "-m", "-M") &&
			!containsLongOptionPrefix(args, "--compile", "--files-from", "--magic-file", "--no-sandbox", "--uncompress", "--uncompress-noreport") &&
			!containsClusteredShortOption(args, "CSfzZmM", "P")
	case "date":
		allowed = len(args) == 0
	case "wc":
		allowed = !containsLongOptionPrefix(args, "--files0-from")
	case "diff":
		// Directory comparison follows nested symlinks by default on BSD/GNU,
		// and pagination can launch `pr`. The built-in diff and /changes paths
		// provide host-confined inspection, so raw diff stays approval-gated.
		allowed = false
	case "mkdir", "touch":
		allowed = len(args) > 0 && !argumentContainsPath(args, "/dev/null")
	case "eslint":
		allowed = !containsArg(args, "--inspect-config", "--init", "--mcp")
	case "prettier":
		allowed = !containsArg(args, "--plugin")
	case "tail":
		allowed = !containsArg(args, "-f", "-F") &&
			!containsLongOptionPrefix(args, "--follow", "--retry") &&
			!containsClusteredShortOption(args, "fF", "")
	case "tsc":
		allowed = autoScopedTSCCommandAllowed(args)
	case "golangci-lint":
		allowed = autoScopedGolangCILintCommandAllowed(args)
	case "gofmt", "staticcheck",
		"pwd", "cat", "head", "uniq", "cut", "tr", "stat", "which", "basename", "dirname", "realpath", "echo", "true", "false", "test", "cmp":
		allowed = true
	default:
		// The executable resolved through the provenance check but has no
		// argument contract in this catalog, so no argument reshuffle can
		// admit it. Report the executable, not the arguments: the generic
		// "arguments outside the host catalog" fall-through invited exactly
		// those futile reshuffles for installed xcrun/node commands.
		assessment.reason = autoCommandReasonExecutableUncatalogued
		return assessment
	}
	if !allowed {
		assessment.reason = autoCommandReasonArguments
		return assessment
	}
	assessment.allowed = true
	assessment.reason = autoCommandReasonAllowed
	return assessment
}

// autoCommandGrantEligibleReason reports the refusals a user-saved bash prefix
// may cure: the segment's executable or its argument form is outside the host
// catalog. Composition refusals (dynamic syntax, splitting, bounds, ambiguous
// pipelines) and path refusals (operands or redirect targets outside the
// workspace) are never grant-curable — the user's rule supplies executable
// authority only, so those checks answer exactly as they do without a grant.
func autoCommandGrantEligibleReason(reason autoCommandReason) bool {
	switch reason {
	case autoCommandReasonExecutable, autoCommandReasonExecutableUncatalogued,
		autoCommandReasonArguments, autoCommandReasonHostToolAvailable:
		return true
	default:
		return false
	}
}

// grantAuthorizedSegmentAssessment re-assesses one catalog-refused segment
// under the user's saved bash prefixes. A match replaces only the catalog's
// executable/argument verdict; everything the host owns still applies here:
//
//   - provenance: the executable must be a bare name that resolves through
//     autoCommandExecutableAllowed, so PATH reaching a workspace-resident or
//     workspace-symlinked binary stays refused even under a grant — otherwise
//     a grant for "x" plus a planted ./x would run repository code with AUTO
//     authority — and a path-qualified spelling never matches at all;
//   - path authority: every operand goes through the same workspace
//     resolution catalogued commands use, with read grants still excluded
//     from raw shell.
//
// The effect is workspace execution — the widest class the catalog itself
// assigns — because a granted executable's real behavior is whatever the user
// vouched for, not something argv inspection can narrow.
func (a *Agent) grantAuthorizedSegmentAssessment(words []string, baseDir string) (autoSimpleCommandAssessment, bool) {
	refused := autoSimpleCommandAssessment{}
	for len(words) > 0 && autoCommandAssignmentAllowed(words[0]) {
		words = words[1:]
	}
	if len(words) == 0 {
		return refused, false
	}
	executable := words[0]
	if filepath.Base(executable) != executable {
		return refused, false
	}
	if !a.autoCommandExecutableAllowed(executable) {
		return refused, false
	}
	if !a.userBashGrantCoversSegment(words) {
		return refused, false
	}
	args := words[1:]
	for index, word := range args {
		if autoCommandNonPathArgument(executable, args, index) {
			continue
		}
		if a.autoCommandPathAssessment(word, baseDir, false) == autoPathDenied {
			return refused, false
		}
	}
	if _, ok := a.autoScopedAttachedPathOptionsAssessment(executable, args, baseDir, false); !ok {
		return refused, false
	}
	return autoSimpleCommandAssessment{
		allowed: true,
		effect:  autoCommandEffectWorkspaceExecution,
		reason:  autoCommandReasonAllowed,
	}, true
}

// userBashGrantCoversSegment consults both grant surfaces the approval flow
// writes: process-local session bash-prefix grants for this workspace, and the
// durable workspace rules. Assignments were already stripped by the caller so
// the pattern's first field lines up with the executable word.
func (a *Agent) userBashGrantCoversSegment(words []string) bool {
	workspace := a.approvalScopeWorkspace()
	a.mu.RLock()
	patterns := make([]string, 0, len(a.approvalGrants))
	for key := range a.approvalGrants {
		parts := strings.Split(key, "\x00")
		if len(parts) >= 4 && parts[0] == workspace && parts[1] == "bash" &&
			parts[2] == permissionpkg.ScopeSessionBashPrefix {
			patterns = append(patterns, parts[3])
		}
	}
	rules := a.workspaceRules
	a.mu.RUnlock()
	for _, pattern := range patterns {
		if permissionpkg.BashSegmentPatternMatches(words, pattern) {
			return true
		}
	}
	return rules.AllowsBashSegment(words)
}

func autoCommandNonPathArgument(executable string, args []string, index int) bool {
	if index < 0 || index >= len(args) {
		return false
	}
	if executable == "go" && len(args) > 0 && args[0] == "test" {
		argument := args[index]
		if strings.HasPrefix(argument, "-run=") || strings.HasPrefix(argument, "--run=") {
			return true
		}
		return index > 0 && stringIn(args[index-1], "-run", "--run")
	}
	if executable == "rg" || executable == "grep" {
		return autoSearchPatternArgument(args, index, executable == "rg")
	}
	if executable == "sed" {
		return autoSedProgramArgument(args, index)
	}
	return false
}

// autoSedProgramArgument distinguishes sed PROGRAM text — an address/command
// script such as `/<\/style>/,/<\/html>/p` — from filesystem operands. The
// generic operand loop read the leading slash of an address regex as an
// absolute path and reported "operand outside the workspace", sending the
// model into path shuffles that could never succeed (5 of 34 refusals in one
// audited session). rg/grep pattern text already has this exemption
// (autoSearchPatternArgument); sed programs are data in exactly the same way.
//
// The exemption decides only which words face path authority; admission still
// belongs to autoScopedSedCommandAllowed, which keeps refusing every
// non-print program (`w` can create files, some dialects execute commands).
//
// sed's grammar makes the classification tractable: with no -e/-f (or their
// long forms) the first non-option word IS the program — never an input file —
// and with -e/-f present there is no positional program at all, so only an
// explicit -e/--expression value is program text. A -f value names a script
// FILE and deliberately stays a path operand.
func autoSedProgramArgument(args []string, target int) bool {
	if sedHasExpressionOrFileOption(args) {
		expectProgram := false
		endOptions := false
		for index, argument := range args {
			if expectProgram {
				expectProgram = false
				if index == target {
					return true
				}
				continue
			}
			if !endOptions && argument == "--" {
				endOptions = true
				continue
			}
			if endOptions || !strings.HasPrefix(argument, "-") || argument == "-" {
				continue
			}
			switch {
			case argument == "-e" || argument == "--expression":
				expectProgram = true
			case strings.HasPrefix(argument, "-e") || strings.HasPrefix(argument, "--expression="):
				if index == target {
					return true
				}
			}
		}
		return false
	}
	endOptions := false
	for index, argument := range args {
		if !endOptions && argument == "--" {
			endOptions = true
			continue
		}
		if !endOptions && strings.HasPrefix(argument, "-") && argument != "-" {
			continue
		}
		// First positional word: the program. Everything after it is an input
		// file and keeps full path authority.
		return index == target
	}
	return false
}

func sedHasExpressionOrFileOption(args []string) bool {
	for _, argument := range args {
		if argument == "--" {
			return false
		}
		if strings.HasPrefix(argument, "-e") || strings.HasPrefix(argument, "-f") ||
			argument == "--expression" || strings.HasPrefix(argument, "--expression=") ||
			argument == "--file" || strings.HasPrefix(argument, "--file=") {
			return true
		}
	}
	return false
}

// autoSearchPatternArgument distinguishes regex text from filesystem operands
// for rg/grep. A leading slash in a pattern is data, not an external read; file
// operands and path-taking options still pass through workspace resolution.
func autoSearchPatternArgument(args []string, target int, ripgrep bool) bool {
	// GNU/BSD option parsing permits pattern options after positional operands.
	// If -e/-f (or rg --files mode) exists, every bare positional is a file path;
	// only an explicit -e value is inline pattern data.
	patternSeen := searchHasNoPositionalPattern(args, ripgrep)
	endOptions := false
	nextValue := byte(0) // 'p' pattern, 'o' other option value
	for index, argument := range args {
		if nextValue != 0 {
			if nextValue == 'p' {
				if index == target {
					return true
				}
				patternSeen = true
			}
			nextValue = 0
			continue
		}
		if !endOptions && argument == "--" {
			endOptions = true
			continue
		}
		if !endOptions && strings.HasPrefix(argument, "--") {
			name, _, attached := strings.Cut(argument, "=")
			if name == "--regexp" {
				if attached {
					if index == target {
						return true
					}
					patternSeen = true
				} else {
					nextValue = 'p'
				}
				continue
			}
			if !attached && searchLongOptionTakesValue(name, ripgrep) {
				nextValue = 'o'
			}
			continue
		}
		if !endOptions && len(argument) > 1 && argument[0] == '-' {
			cluster := argument[1:]
			for offset, option := range cluster {
				if option == 'e' {
					if offset+1 < len(cluster) {
						if index == target {
							return true
						}
						patternSeen = true
					} else {
						nextValue = 'p'
					}
					break
				}
				if searchShortOptionTakesValue(option, ripgrep) {
					if offset+1 == len(cluster) {
						nextValue = 'o'
					}
					break
				}
			}
			continue
		}
		if !patternSeen {
			if index == target {
				return true
			}
			patternSeen = true
		}
	}
	return false
}

func searchHasNoPositionalPattern(args []string, ripgrep bool) bool {
	endOptions := false
	skipNext := false
	for _, argument := range args {
		if skipNext {
			skipNext = false
			continue
		}
		if !endOptions && argument == "--" {
			endOptions = true
			continue
		}
		if endOptions {
			continue
		}
		if strings.HasPrefix(argument, "--") {
			name, _, attached := strings.Cut(argument, "=")
			if name == "--regexp" || name == "--file" || ripgrep && name == "--files" {
				return true
			}
			if !attached && searchLongOptionTakesValue(name, ripgrep) {
				skipNext = true
			}
			continue
		}
		if len(argument) <= 1 || argument[0] != '-' {
			continue
		}
		cluster := argument[1:]
		for offset, option := range cluster {
			if option == 'e' || option == 'f' {
				return true
			}
			if searchShortOptionTakesValue(option, ripgrep) {
				if offset+1 == len(cluster) {
					skipNext = true
				}
				break
			}
		}
	}
	return false
}

func searchShortOptionTakesValue(option rune, ripgrep bool) bool {
	if ripgrep {
		return strings.ContainsRune("ABCEMTdfgjmrt", option)
	}
	return strings.ContainsRune("ABCDdfm", option)
}

func searchLongOptionTakesValue(name string, ripgrep bool) bool {
	common := []string{"--after-context", "--before-context", "--context", "--regexp", "--file", "--max-count"}
	if stringIn(name, common...) {
		return true
	}
	if ripgrep {
		return stringIn(name,
			"--encoding", "--max-columns", "--type-not", "--max-depth", "--glob", "--threads",
			"--replace", "--type", "--ignore-file", "--sort", "--sortr", "--path-separator",
			"--field-context-separator", "--field-match-separator", "--engine", "--colors",
			"--hyperlink-format", "--max-filesize",
		)
	}
	return stringIn(name,
		"--devices", "--directories", "--exclude", "--exclude-from", "--include", "--label", "--binary-files",
	)
}

// autoCommandHasBuiltinSearchRemedy reports whether a refused executable names
// a recursive search or directory enumeration that the built-in grep, glob, ls
// and read tools serve directly. It is deliberately name-based and PATH-free:
// this decides an explanation, never an admission.
//
// The same names appear in the executable switch in
// assessAutoScopedSimpleCommand, which is where an installed one lands.
// TestSearchRefusalNamesTheToolThatWouldWork walks every name through the
// public entry point, so the two lists cannot silently drift apart.
// autoGrepWalksDirectories reports whether this grep invocation can reach a
// file it was not handed, which is the only way a search escapes the
// per-operand workspace and secret-policy checks.
//
// Three families do it, and nothing else in grep's grammar does:
//
//   - recursion (-r, -R, --recursive, --dereference-recursive), including
//     inside a POSIX short cluster such as -rn;
//   - --directories / -d in any form, because `-d recurse` is a spelling of
//     recursion and the other modes are not worth distinguishing here;
//   - --include / --exclude / --exclude-dir / --exclude-from, which only have
//     meaning while walking and whose presence signals a form this admission
//     was not reasoned about.
//
// Everything else grep accepts changes what it matches or how it prints, never
// which files it opens.
func autoGrepWalksDirectories(args []string) bool {
	if containsArg(args, "-r", "-R", "-d", "--recursive", "--dereference-recursive", "--directories") ||
		containsLongOptionPrefix(args,
			"--recursive", "--dereference-recursive", "--directories",
			"--include", "--exclude", "--exclude-dir", "--exclude-from") {
		return true
	}
	// A short cluster hides the same options: -rn recurses, and -dr takes
	// "recurse" from the next word. containsClusteredShortOption already knows
	// that a value-taking letter ends the cluster it can speak for.
	return containsClusteredShortOption(args, "rR", "ABCDdfm") ||
		containsClusteredShortOption(args, "d", "ABCDfm")
}

func autoCommandHasBuiltinSearchRemedy(executable string) bool {
	return stringIn(executable, "find", "rg", "grep", "tree", "du", "ls")
}

func (a *Agent) autoCommandExecutableAllowed(executable string) bool {
	if stringIn(executable, "cd", "echo", "false", "printf", "pwd", "test", "true") {
		return true
	}
	resolved, err := exec.LookPath(executable)
	if err != nil || !filepath.IsAbs(resolved) {
		return false
	}
	// Reject both a lexical workspace executable and an external symlink whose
	// physical target is inside the workspace. The shell may still prefer a
	// builtin for names above, but every catalogued external command must resolve
	// through a host path the agent cannot create with confined writes.
	if root := strings.TrimSpace(a.activeWorkDir()); root != "" {
		absoluteRoot, rootErr := filepath.Abs(root)
		absoluteResolved, resolvedErr := filepath.Abs(resolved)
		if rootErr != nil || resolvedErr != nil {
			return false
		}
		if _, inside, relativeErr := workspaceRelative(absoluteRoot, absoluteResolved); relativeErr != nil || inside {
			return false
		}
	}
	return !a.pathWithinWorkspace(resolved)
}

func autoCommandEffectForExecutable(executable string, args []string) autoCommandEffect {
	switch executable {
	case "mkdir", "touch":
		return autoCommandEffectWorkspaceMutation
	case "sort":
		if autoSortWritesOutput(args) {
			return autoCommandEffectWorkspaceMutation
		}
		return autoCommandEffectReadOnly
	case "gofmt":
		if containsArg(args, "-w") {
			return autoCommandEffectWorkspaceMutation
		}
		return autoCommandEffectReadOnly
	case "go", "npm", "pnpm", "yarn", "bun", "cargo", "swift", "eslint", "prettier", "tsc", "golangci-lint", "staticcheck":
		return autoCommandEffectWorkspaceExecution
	default:
		return autoCommandEffectReadOnly
	}
}

func autoSortWritesOutput(args []string) bool {
	for _, argument := range args {
		if argument == "-o" || strings.HasPrefix(argument, "--out") {
			return true
		}
		if len(argument) > 2 && argument[0] == '-' && argument[1] != '-' && strings.ContainsRune(argument[1:], 'o') {
			return true
		}
	}
	return false
}

func autoScopedPrintfCommandAllowed(args []string) bool {
	for _, argument := range args {
		// printf is commonly a shell builtin. In Bash and Zsh, -v assigns a shell
		// variable; changing PATH can make the following catalogued command resolve
		// to attacker-controlled workspace code.
		if argument == "-v" || strings.HasPrefix(argument, "-v") {
			return false
		}
	}
	return true
}

func (a *Agent) autoScopedAttachedPathOptionsAssessment(executable string, args []string, baseDir string, allowReadGrants bool) (autoPathAuthority, bool) {
	var options []string
	switch executable {
	case "find", "make", "just", "rg", "grep":
		options = []string{"-f"}
	case "file":
		options = []string{"-f", "-m"}
	case "touch":
		options = []string{"-r"}
	case "task":
		options = []string{"-t", "-d"}
	case "sort":
		options = []string{"-o", "-T"}
	case "tree":
		options = []string{"-o"}
	case "golangci-lint":
		options = []string{"-c"}
	case "eslint":
		options = []string{"-c", "-o"}
	case "tsc":
		options = []string{"-p"}
	}
	if executable == "make" {
		options = append(options, "-C", "-I")
	}
	if executable == "just" {
		options = append(options, "-d")
	}
	for _, argument := range args {
		value, attached := clusteredShortOptionValue(argument, options)
		if !attached {
			continue
		}
		authority := a.autoCommandPathAssessment(value, baseDir, allowReadGrants)
		if authority == autoPathDenied {
			return autoPathDenied, false
		}
		if authority == autoPathReadGrant {
			return autoPathReadGrant, true
		}
	}
	return autoPathWorkspace, true
}

func clusteredShortOptionValue(argument string, options []string) (string, bool) {
	if len(argument) < 3 || argument[0] != '-' || argument[1] == '-' {
		return "", false
	}
	cluster := argument[1:]
	for index := range cluster {
		option := "-" + string(cluster[index])
		if !stringIn(option, options...) {
			continue
		}
		value := strings.TrimPrefix(cluster[index+1:], "=")
		if value == "" {
			return "", false
		}
		return value, true
	}
	return "", false
}

// autoPackageRunScript is the conservative shape of a manifest script name:
// letters, digits, dash, underscore, and the conventional namespace colon
// (`site:build`). It deliberately cannot start with `-` (an option to the
// runner), contain `/` or `.` (a path rather than a manifest entry), or carry
// whitespace (quoted multi-word tokens).
var autoPackageRunScript = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_:-]{0,127}$`)

// autoScopedPackageCommandAllowed admits a runner's catalogued direct
// subcommands plus exactly `run <script>`. The script NAME grants no new
// authority: `npm test` and `bun test` already execute whatever the workspace
// manifest defines for those names, so refusing `bun run lint` while admitting
// `bun test` only moved the same trust decision behind an approval prompt —
// in one audited AUTO session those refusals surfaced as unexplained
// "arguments outside the host catalog" prompts for routine lint/build scripts.
// The argv shape IS the boundary and stays strict: `--` passthrough and any
// extra argument reach the interpreter or the script with flags the catalog
// never inspected, and the charset keeps options, paths, and quoted spaces out
// of the script slot.
func autoScopedPackageCommandAllowed(args []string, direct ...string) bool {
	if len(args) == 1 && firstArgIn(args, direct...) {
		return true
	}
	return len(args) == 2 && args[0] == "run" && autoPackageRunScript.MatchString(args[1])
}

func autoScopedGoCommandAllowed(args []string) bool {
	if !firstArgIn(args, "build", "test", "vet", "list", "env", "version", "doc", "fmt") &&
		(len(args) < 2 || args[0] != "mod" || !stringIn(args[1], "tidy", "verify", "why", "graph")) {
		return false
	}
	// These options either mutate user-global Go configuration or delegate to
	// another executable. They need an explicit decision even when the outer Go
	// command itself is routine.
	return !containsArgumentNamePrefix(args, "-test.fuzz", "--test.fuzz") && !containsArg(args,
		"-w", "-u", "-exec", "--exec", "-toolexec", "--toolexec", "-vettool", "--vettool",
		"-ldflags", "--ldflags", "-gccgoflags", "--gccgoflags", "-gcflags", "--gcflags", "-asmflags", "--asmflags",
		"-fuzz", "--fuzz",
	)
}

// autoScopedGitCommandAllowed admits the git subcommands that inspect history
// or working-tree state, and only in argument forms that cannot write a file or
// hand control to another program.
//
// Git was previously excluded outright. That made orienting — the single most
// common thing an agent does before touching anything — cost an interactive
// approval, and because dispatch preserves model order, one `git status` stalled
// every read-only tool queued behind it in the same batch.
//
// Subcommands are allowlisted, never denylisted, because too many git verbs read
// in one argument form and destroy in another: `branch -D` deletes, a bare
// `tag <name>` creates, `config k v` writes, a bare `stash` pushes the worktree.
// None of those appear here, and adding one requires proving it cannot mutate
// under ANY accepted argument.
//
// That "under ANY accepted argument" proof was originally attempted with a flag
// denylist, and a denylist cannot carry it. It has now failed three times, twice
// for the same structural reason: --textconv was missing, and then -S<file> and
// -O<file> were admitted because a denied option is only recognized as its own
// token or =-joined, while git also accepts the value attached to the letter.
// The generic operand loop cannot see through that either — it reads
// `-S/etc/passwd` as one relative word and joins it under the workspace. So the
// short-option surface is now an allowlist too: autoGitShortOptionTokenAllowed
// refuses every attached-value token that is not provably value-free, which is
// what makes the proof obligation above discharge for options nobody enumerated.
//
// A global option before the subcommand (-c, -C, --git-dir, --work-tree,
// --exec-path, --config-env) is refused implicitly: args[0] must itself be a
// catalogued subcommand, and such an option would occupy that slot. This matters
// more than it looks — `git -c diff.external=... diff` turns a read into an
// arbitrary execution. Aliases need no handling: git does not permit an alias to
// shadow a built-in subcommand.
//
// Caveat worth stating: a repository or user gitconfig that already sets
// diff.external or core.pager will run that program under `git diff` with no
// flag at all. That is the operator's own configured tooling, not something a
// model can introduce here, and it is equally true of everything else on PATH.
func autoScopedGitCommandAllowed(args []string) bool {
	if !firstArgIn(args,
		"status", "log", "show", "diff", "rev-parse", "ls-files", "blame", "describe", "shortlog",
	) {
		return false
	}
	// --output makes diff/show write a file; --ext-diff, --extcmd, and
	// --textconv each hand the content to an external program named by config;
	// --no-index escapes the repository into arbitrary filesystem paths.
	// containsLongOptionPrefix also refuses unique-prefix abbreviations such as
	// --out for --output.
	if containsLongOptionPrefix(args, "--output", "--ext-diff", "--extcmd", "--textconv", "--no-index") {
		return false
	}
	// Short forms the long-option guard cannot see.
	if containsArg(args, "-o", "-O") {
		return false
	}
	return autoGitShortOptionsAllowed(args)
}

// Short options git accepts with the value attached to the letter, across the
// catalogued read subcommands, name files (-O<orderfile>, -o<output>,
// blame's -S<revs-file>, ls-files' -X<exclude-from>), regular expressions
// (-G, -I, -S as diff/log's pickaxe), and ranges (-L). The meaning is not even
// stable per letter: -S is a pickaxe string for log and diff but a revs-FILE
// for blame, and -l is a numeric rename limit for diff but a boolean for blame.
// Enumerating them per subcommand is exactly the exercise that already leaked
// twice, so the direction is inverted: an attached-value token is refused
// unless it is provably free of a value.
//
// The allowlist below admits only letters, digits, and the similarity `%`. That
// charset is the actual security property, and it holds regardless of whether
// every letter below was classified correctly: a token built from it cannot
// contain `/`, `.`, `~`, or `=`, so it cannot express an absolute path, a
// parent traversal, a home expansion, or an =-joined value. Whatever a
// misclassified letter attaches can therefore only resolve inside the
// workspace, where the operand loop already grants authority.
const (
	// Short options that never consume a value under any catalogued
	// subcommand, so they may appear anywhere in a POSIX cluster (`-sn`).
	autoGitValuelessShortOptions = "abcdefghikmpqrstvwzDERW"
	// Short options whose value, when present, is numeric: -U<n> context,
	// -M<n>/-C<n>/-B<n> similarity, -l<num> rename limit, -n<num> commit
	// count. Each is also valid bare, which is how blame's boolean -l and -n
	// and `shortlog -sn` keep working.
	autoGitNumericShortOptions = "BCMUln"
)

// autoGitUntrackedModes are the fixed enum values `git status -u<mode>` takes.
// They are spelled out because the mode rides attached to the letter far more
// often than not (`git status -uno`).
var autoGitUntrackedModes = []string{"no", "normal", "all"}

func autoGitShortOptionsAllowed(args []string) bool {
	for _, argument := range args {
		// Deliberately no `--` early exit. git stops parsing options there, so a
		// later `-S…` word is a pathspec rather than a revs-file — but that is a
		// per-subcommand claim about git's own argument grammar (blame's operand
		// order differs from log's), and this guard exists precisely because
		// per-subcommand claims about git's grammar have been wrong twice. A
		// pathspec that looks like an attached short option costs one approval.

		// A two-character token carries no attached value. Its detached value,
		// if any, is a separate word that the operand loop resolves — which is
		// why `git log -S needle` and `git blame -L 1,10 file` still pass while
		// `git blame -S /etc/passwd` does not.
		if len(argument) < 3 || argument[0] != '-' || argument[1] == '-' {
			continue
		}
		if !autoGitShortOptionTokenAllowed(argument) {
			return false
		}
	}
	return true
}

// autoGitShortOptionTokenAllowed reports whether one `-xyz` token is safe
// without knowing which subcommand will interpret it.
func autoGitShortOptionTokenAllowed(argument string) bool {
	cluster := argument[1:]
	if autoGitAllDigits(cluster) {
		// `git log -10`: a bare commit count, not an option letter at all.
		return true
	}
	for index := 0; index < len(cluster); index++ {
		option := cluster[index]
		switch {
		case strings.IndexByte(autoGitValuelessShortOptions, option) >= 0:
			continue
		case strings.IndexByte(autoGitNumericShortOptions, option) >= 0:
			end := index + 1
			for end < len(cluster) && (autoGitDigit(cluster[end]) || cluster[end] == '%') {
				end++
			}
			index = end - 1
		case option == 'u':
			// `git status -u`, `-uno`, `-uall`. Anything else attached to -u is
			// ambiguous between a mode and more flags, so it fails closed.
			rest := cluster[index+1:]
			if rest == "" {
				continue
			}
			return stringIn(rest, autoGitUntrackedModes...)
		default:
			return false
		}
	}
	return true
}

func autoGitAllDigits(value string) bool {
	if value == "" {
		return false
	}
	for index := 0; index < len(value); index++ {
		if !autoGitDigit(value[index]) {
			return false
		}
	}
	return true
}

func autoGitDigit(character byte) bool {
	return character >= '0' && character <= '9'
}

func autoScopedCargoCommandAllowed(args []string) bool {
	if !firstArgIn(args, "build", "test", "check", "fmt", "clippy", "metadata", "doc", "bench") {
		return false
	}
	// Cargo configuration can select runner/compiler wrapper executables; doc
	// --open launches an external application. Both exceed routine build/test
	// authority even when the primary Cargo subcommand is catalogued.
	return !containsArg(args, "--config", "--open")
}

func autoScopedSwiftCommandAllowed(args []string) bool {
	if !firstArgIn(args, "build", "test") || containsResponseFileOperand(args) {
		return false
	}
	return !containsArg(args, "--disable-sandbox", "-Xcc", "-Xswiftc", "-Xlinker", "-Xcxx")
}

func autoScopedTSCCommandAllowed(args []string) bool {
	return !containsResponseFileOperand(args) &&
		!containsArg(args, "-w", "--watch", "--clean", "--typeRoots", "--rootDirs") &&
		!containsClusteredShortOption(args, "w", "")
}

func autoScopedGolangCILintCommandAllowed(args []string) bool {
	if firstArgIn(args, "run", "fmt", "linters", "version") {
		return true
	}
	return len(args) == 2 && args[0] == "config" && args[1] == "verify"
}

func autoScopedSedCommandAllowed(args []string) bool {
	if len(args) < 2 {
		return false
	}
	for len(args) > 0 && stringIn(args[0], "-n", "--quiet", "--silent", "-E", "-r") {
		args = args[1:]
	}
	if len(args) < 2 {
		return false
	}
	// Admit the common read-only inspection form (`sed -n 1,120p file`).
	// General sed programs can use `w` to create files and some variants can
	// execute commands, so they remain approval-gated. GNU getopt may permute a
	// later `-i` or `-e` ahead of the program; reject every post-program option
	// unless an explicit `--` makes the remaining words unambiguous filenames.
	if !autoSedPrintProgram.MatchString(args[0]) {
		return false
	}
	files := args[1:]
	if len(files) > 0 && files[0] == "--" {
		files = files[1:]
		return len(files) > 0
	}
	if len(files) == 0 {
		return false
	}
	for _, file := range files {
		if strings.HasPrefix(file, "-") {
			return false
		}
	}
	return true
}

func autoCommandAssignmentAllowed(assignment string) bool {
	if !autoCommandAssignment.MatchString(assignment) {
		return false
	}
	name, value, _ := strings.Cut(assignment, "=")
	return stringIn(name, "CI", "NO_COLOR", "FORCE_COLOR") && autoCommandAssignmentValue.MatchString(value)
}

func (a *Agent) autoCommandPathAssessment(word, baseDir string, allowReadGrants bool) autoPathAuthority {
	result := a.autoCommandCandidatePathAssessment(word, baseDir, allowReadGrants)
	if result == autoPathDenied {
		return autoPathDenied
	}
	if _, value, found := strings.Cut(word, "="); found {
		valueResult := a.autoCommandCandidatePathAssessment(value, baseDir, allowReadGrants)
		if valueResult == autoPathDenied {
			return autoPathDenied
		}
		if valueResult == autoPathReadGrant {
			result = autoPathReadGrant
		}
	}
	return result
}

func (a *Agent) autoCommandCandidatePathAssessment(candidate, baseDir string, allowReadGrants bool) autoPathAuthority {
	if candidate == "" || candidate == "/dev/null" {
		return autoPathWorkspace
	}
	if strings.HasPrefix(candidate, "~") || rawPathHasParentTraversal(candidate) {
		return autoPathDenied
	}
	if !filepath.IsAbs(candidate) && baseDir != "" {
		candidate = filepath.Join(baseDir, candidate)
	}
	// Resolve even relative operands so a workspace symlink cannot turn an
	// apparently confined shell command into an external read or write.
	if a.pathWithinWorkspace(candidate) {
		return autoPathWorkspace
	}
	if !allowReadGrants {
		return autoPathDenied
	}
	readable, err := a.resolveReadablePath(candidate)
	if err != nil {
		return autoPathDenied
	}
	defer func() { _ = readable.close() }()
	// Exact-file grants are intentionally capable of naming sensitive files for
	// host-owned read tools after explicit consent. Raw shell execution is a
	// weaker boundary, so conventional secret paths stay approval-gated even
	// when an exact read grant exists.
	if config.HostSecretPathIgnored(readable.absolute) {
		return autoPathDenied
	}
	return autoPathReadGrant
}

func rawPathHasParentTraversal(path string) bool {
	for _, component := range strings.FieldsFunc(path, func(character rune) bool {
		return character == '/' || character == '\\'
	}) {
		if component == ".." {
			return true
		}
	}
	return false
}

func (a *Agent) pathWithinWorkspace(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	// Keep shell authority narrower than typed write grants. Explicit external
	// write scopes are consumed by built-in write/edit/mkdir and trusted routed
	// workspace tools; they never make a raw shell operand "workspace-local".
	_, err := a.resolveWorkspacePath(path)
	return err == nil
}

func firstArgIn(args []string, allowed ...string) bool {
	return len(args) > 0 && stringIn(args[0], allowed...)
}

func stringIn(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func containsArg(args []string, denied ...string) bool {
	for _, arg := range args {
		for _, candidate := range denied {
			if arg == candidate || strings.HasPrefix(arg, candidate+"=") {
				return true
			}
		}
	}
	return false
}

// containsLongOptionPrefix fails closed for GNU-style unique long-option
// abbreviations. Several catalogued tools accept prefixes such as
// --files0-fro for --files0-from, so exact string matching is insufficient.
func containsLongOptionPrefix(args []string, denied ...string) bool {
	for _, argument := range args {
		name, _, _ := strings.Cut(argument, "=")
		if !strings.HasPrefix(name, "--") || name == "--" {
			continue
		}
		for _, full := range denied {
			if strings.HasPrefix(full, name) {
				return true
			}
		}
	}
	return false
}

func containsArgumentNamePrefix(args []string, denied ...string) bool {
	for _, argument := range args {
		name, _, _ := strings.Cut(argument, "=")
		for _, prefix := range denied {
			if strings.HasPrefix(name, prefix) {
				return true
			}
		}
	}
	return false
}

func containsResponseFileOperand(args []string) bool {
	for _, argument := range args {
		if strings.HasPrefix(argument, "@") {
			return true
		}
	}
	return false
}

func argumentContainsPath(args []string, path string) bool {
	for _, argument := range args {
		if strings.Contains(argument, path) {
			return true
		}
	}
	return false
}

// containsClusteredShortOption finds denied flags inside POSIX short-option
// clusters (for example, `-bz` and `-Pz`). Once an option that consumes the
// rest of its token is encountered, later bytes are its value rather than
// flags and are deliberately ignored.
func containsClusteredShortOption(args []string, denied, valueTaking string) bool {
	for _, argument := range args {
		if argument == "--" {
			return false
		}
		if len(argument) < 3 || argument[0] != '-' || argument[1] == '-' {
			continue
		}
		for _, option := range argument[1:] {
			if strings.ContainsRune(denied, option) {
				return true
			}
			if strings.ContainsRune(valueTaking, option) {
				break
			}
		}
	}
	return false
}
