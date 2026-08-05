package ui

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/viewport"
	"charm.land/lipgloss/v2"

	"github.com/abdul-hamid-achik/sonar/internal/permission"
)

const (
	approvalMaximumActionBytes      = 256
	approvalMaximumConsequenceBytes = 768
)

// ApprovalState owns presentation-only state for an approval request. The
// root Model remains responsible for every decision and side effect.
type ApprovalState struct {
	Viewport      viewport.Model
	ShowArguments bool
	ChoiceIndex   int
}

type approvalChoice struct {
	Decision     permission.ApprovalDecision
	Key          string
	Label        string
	CompactLabel string
	// ScopeKind narrows an AllowSession choice. Empty means exact_request.
	ScopeKind string
}

// Keep the safe outcome first: Enter on a newly opened permission request
// denies it unless the user deliberately moves to an allow choice. Direct
// y/n/s (and a when offered) shortcuts remain available regardless of focus.
var baseApprovalChoices = [...]approvalChoice{
	{Decision: permission.DecisionUserDeny, Key: "n", Label: "deny", CompactLabel: "deny"},
	{Decision: permission.DecisionAllowOnce, Key: "y", Label: "once", CompactLabel: "once"},
	{
		Decision:     permission.DecisionAllowSession,
		Key:          "s",
		Label:        "same request again this session",
		CompactLabel: "same/session",
	},
}

// approvalChoicesFor returns the decision rows for a tool. Wider session
// scopes are offered only when the tool can bind them safely. Labels include
// derived path/prefix hints when available so the user sees what "a"/"p"/"w" mean.
func approvalChoicesFor(toolName string, preview permission.ApprovalPreview) []approvalChoice {
	choices := make([]approvalChoice, 0, len(baseApprovalChoices)+3)
	choices = append(choices, baseApprovalChoices[:]...)
	switch {
	case sessionToolScopeEligibleUI(toolName):
		choices = append(choices, approvalChoice{
			Decision:     permission.DecisionAllowSession,
			Key:          "a",
			Label:        "this tool again this session",
			CompactLabel: "tool/session",
			ScopeKind:    permission.ScopeSessionTool,
		})
		if path := strings.TrimSpace(preview.Path); path != "" {
			pathHint := compactApprovalHint(path, 28)
			pathLabel := "this path again this session"
			pathCompact := "path/session"
			if pathHint != "" {
				pathLabel = "path again this session · " + pathHint
				pathCompact = "path · " + pathHint
			}
			choices = append(choices, approvalChoice{
				Decision:     permission.DecisionAllowSession,
				Key:          "p",
				Label:        pathLabel,
				CompactLabel: pathCompact,
				ScopeKind:    permission.ScopeSessionPath,
			})
			saveLabel := "save this path for this workspace"
			saveCompact := "path/workspace"
			if pathHint != "" {
				saveLabel = "save path for workspace · " + pathHint
				saveCompact = "save · " + pathHint
			}
			choices = append(choices, approvalChoice{
				Decision:     permission.DecisionAllowSession,
				Key:          "w",
				Label:        saveLabel,
				CompactLabel: saveCompact,
				ScopeKind:    permission.DurableWritePath,
			})
		}
	case toolName == "bash":
		if prefix, ok := permission.ApprovalBashPrefix(preview, ""); ok {
			prefixHint := compactApprovalHint(prefix, 24)
			sessionLabel := "command prefix this session"
			sessionCompact := "bash/session"
			if prefixHint != "" {
				sessionLabel = "prefix this session · " + prefixHint
				sessionCompact = "bash · " + prefixHint
			}
			choices = append(choices, approvalChoice{
				Decision:     permission.DecisionAllowSession,
				Key:          "a",
				Label:        sessionLabel,
				CompactLabel: sessionCompact,
				ScopeKind:    permission.ScopeSessionBashPrefix,
			})
			// Durable form uses trailing * when the approved command has more args.
			durable := prefix
			if fields := strings.Fields(strings.TrimSpace(preview.Command)); len(fields) > len(strings.Fields(prefix)) {
				durable = prefix + " *"
			}
			durableHint := compactApprovalHint(durable, 24)
			saveLabel := "save bash pattern for this workspace"
			saveCompact := "bash/workspace"
			if durableHint != "" {
				saveLabel = "save for workspace · " + durableHint
				saveCompact = "save · " + durableHint
			}
			choices = append(choices, approvalChoice{
				Decision:     permission.DecisionAllowSession,
				Key:          "w",
				Label:        saveLabel,
				CompactLabel: saveCompact,
				ScopeKind:    permission.DurableBashPrefix,
			})
		}
	case sessionMCPToolScopeEligibleUI(toolName):
		toolHint := compactApprovalHint(toolName, 28)
		sessionLabel := "this MCP tool again this session"
		sessionCompact := "mcp/session"
		if toolHint != "" {
			sessionLabel = "MCP tool this session · " + toolHint
			sessionCompact = "mcp · " + toolHint
		}
		choices = append(choices, approvalChoice{
			Decision:     permission.DecisionAllowSession,
			Key:          "a",
			Label:        sessionLabel,
			CompactLabel: sessionCompact,
			ScopeKind:    permission.ScopeSessionMCPTool,
		})
		saveLabel := "save MCP tool for this workspace"
		saveCompact := "mcp/workspace"
		if toolHint != "" {
			saveLabel = "save MCP for workspace · " + toolHint
			saveCompact = "save · " + toolHint
		}
		choices = append(choices, approvalChoice{
			Decision:     permission.DecisionAllowSession,
			Key:          "w",
			Label:        saveLabel,
			CompactLabel: saveCompact,
			ScopeKind:    permission.DurableMCPTool,
		})
	}
	return choices
}

func compactApprovalHint(value string, limit int) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if strings.Contains(value, "/") || strings.Contains(value, `\`) {
		value = filepath.Base(value)
	}
	if limit > 0 && len(value) > limit {
		if limit <= 3 {
			return value[:limit]
		}
		return value[:limit-3] + "..."
	}
	return value
}

func sessionToolScopeEligibleUI(toolName string) bool {
	switch strings.TrimSpace(toolName) {
	case "write", "edit", "mkdir":
		return true
	default:
		return false
	}
}

func sessionMCPToolScopeEligibleUI(toolName string) bool {
	_, ok := permission.NormalizeMCPToolName(toolName)
	return ok
}

func (m *Model) currentApprovalChoices() []approvalChoice {
	toolName := ""
	var preview permission.ApprovalPreview
	if m != nil && m.pendingApproval != nil {
		toolName = m.pendingApproval.ToolName
		preview = m.pendingApproval.Preview
		if preview.Command == "" {
			if cmd, ok := m.pendingApproval.Args["command"].(string); ok {
				preview.Command = cmd
			}
		}
		if preview.Path == "" {
			if path, ok := m.pendingApproval.Args["path"].(string); ok {
				preview.Path = path
			}
		}
	}
	return approvalChoicesFor(toolName, preview)
}

type approvalTranscriptAnchor struct {
	valid   bool
	paused  bool
	yOffset int
}

func (m *Model) captureApprovalTranscriptAnchor() approvalTranscriptAnchor {
	if m == nil || !m.ready {
		return approvalTranscriptAnchor{}
	}
	return approvalTranscriptAnchor{
		valid:   true,
		paused:  m.followPaused(),
		yOffset: m.transcriptYOffset(),
	}
}

func (m *Model) restoreApprovalTranscriptAnchor(anchor approvalTranscriptAnchor) {
	if m == nil || !anchor.valid {
		return
	}
	m.restoreFollowPosition(anchor.paused, anchor.yOffset)
}

func (m *Model) openApproval(request ToolApprovalMsg) error {
	if strings.TrimSpace(request.ToolName) == "" {
		return fmt.Errorf("tool identity is missing")
	}
	if request.Response == nil {
		return fmt.Errorf("approval response channel is unavailable")
	}
	if _, err := json.Marshal(request.Args); err != nil {
		return fmt.Errorf("encode exact arguments: %w", err)
	}

	m.preemptTranscriptSearch()
	anchor := m.captureApprovalTranscriptAnchor()
	m.clearViewerModals(false)
	// Approval has the highest input authority. If a host request arrives while
	// a composer-owned form is visible, cancel that transient form instead of
	// retaining inaccessible state behind the decision surface.
	if m.overlay == OverlayPlanForm || m.overlay == OverlayGoalForm {
		m.planFormState = nil
		m.goalFormState = nil
		m.overlay = OverlayNone
	}
	// A Cortex decision remains presentation-only while approval temporarily
	// owns the higher-priority inline surface. Its durable blocker is unchanged,
	// and the exact request is shown again after approval settles.
	if m.overlay == OverlayCortexDecision {
		m.overlay = OverlayNone
	}
	m.pendingApproval = &request
	m.approvalState = &ApprovalState{ChoiceIndex: 0}
	// Permission decisions are part of the active conversation flow. They own
	// the composer/footer region instead of covering the transcript with a
	// centered modal. Clear a completion defensively so an asynchronous host
	// request never leaves hidden filter state behind the inline surface.
	if m.isCompletionActive() {
		m.dismissCompletion()
	}
	m.overlayParent = OverlayNone
	m.overlay = OverlayNone
	m.input.Blur()
	m.resizeApproval(false)
	m.recalcViewportHeight()
	m.restoreApprovalTranscriptAnchor(anchor)
	return nil
}

func (m *Model) resolvePendingApproval(response permission.ApprovalResponse) {
	m.resolvePendingApprovalWithScope(response, "")
}

func (m *Model) resolvePendingApprovalWithScope(response permission.ApprovalResponse, scopeKind string) {
	anchor := m.captureApprovalTranscriptAnchor()
	if scopeKind == permission.DurableBashPrefix || scopeKind == permission.DurableMCPTool || scopeKind == permission.DurableWritePath {
		if err := m.persistWorkspaceRuleFromApproval(scopeKind); err != nil {
			m.entries = append(m.entries, ChatEntry{
				Kind:    "error",
				Content: "Could not save workspace rule: " + err.Error(),
			})
		} else {
			m.entries = append(m.entries, ChatEntry{
				Kind:    "system",
				Content: "Saved durable workspace rule. Future sessions in this workspace will reuse it until removed with /permissions.",
			})
		}
	}
	if m.pendingApproval != nil && m.pendingApproval.Response != nil {
		m.pendingApproval.Response <- response
	}
	m.pendingApproval = nil
	m.approvalState = nil
	if m.composerEditable() {
		m.input.Focus()
	} else {
		m.input.Blur()
	}
	m.recalcViewportHeight()
	m.restoreApprovalTranscriptAnchor(anchor)
	m.activateCortexDecision()
}

func (m *Model) toggleApprovalDetails() {
	if m.approvalState == nil || m.pendingApproval == nil {
		return
	}
	anchor := m.captureApprovalTranscriptAnchor()
	m.approvalState.ShowArguments = !m.approvalState.ShowArguments
	m.resizeApproval(false)
	m.recalcViewportHeight()
	m.restoreApprovalTranscriptAnchor(anchor)
}

func (m *Model) moveApprovalChoice(delta int) {
	choices := m.currentApprovalChoices()
	if m.approvalState == nil || len(choices) == 0 || delta == 0 {
		return
	}
	index := m.approvalState.ChoiceIndex + delta
	if index < 0 {
		index = len(choices) - 1
	}
	if index >= len(choices) {
		index = 0
	}
	m.approvalState.ChoiceIndex = index
}

func (m *Model) resetHiddenApprovalChoice() {
	if m != nil && m.approvalState != nil {
		m.approvalState.ChoiceIndex = 0
	}
}

func (m *Model) selectedApprovalResponseAndScope() (permission.ApprovalResponse, string) {
	choices := m.currentApprovalChoices()
	if m.approvalState == nil || m.approvalState.ChoiceIndex < 0 ||
		m.approvalState.ChoiceIndex >= len(choices) {
		return permission.Deny(), ""
	}
	choice := choices[m.approvalState.ChoiceIndex]
	switch choice.Decision {
	case permission.DecisionAllowOnce:
		return permission.AllowOnce(), ""
	case permission.DecisionAllowSession:
		return approvalResponseForScope(choice.ScopeKind), choice.ScopeKind
	default:
		return permission.Deny(), ""
	}
}

func approvalResponseForScope(scopeKind string) permission.ApprovalResponse {
	switch scopeKind {
	case permission.ScopeSessionTool:
		return permission.AllowSessionTool()
	case permission.ScopeSessionPath:
		return permission.AllowSessionPath()
	case permission.ScopeSessionBashPrefix, permission.DurableBashPrefix:
		// Workspace durable save is applied by the UI before the response is
		// delivered; the agent still receives a process-local bash-prefix grant.
		return permission.AllowSessionBashPrefix()
	case permission.ScopeSessionMCPTool, permission.DurableMCPTool:
		return permission.AllowSessionMCPTool()
	case permission.DurableWritePath:
		// Durable path save + process-local path grant for this turn.
		return permission.AllowSessionPath()
	default:
		return permission.AllowSession()
	}
}

// persistWorkspaceRuleFromApproval writes durable rules when the user chose
// the workspace ("w") option. Failures are returned for system messaging; the
// session grant still proceeds via the response.
func (m *Model) persistWorkspaceRuleFromApproval(scopeKind string) error {
	if m == nil || m.agent == nil || m.pendingApproval == nil {
		return nil
	}
	switch scopeKind {
	case permission.DurableBashPrefix:
		command := m.pendingApproval.Preview.Command
		if command == "" {
			command, _ = m.pendingApproval.Args["command"].(string)
		}
		// Prefer a trailing-glob form so "go test ./pkg" variants keep matching.
		// The prefix itself comes from the same resolver the modal labelled and
		// the session grant will use, so "w" durably saves what "a" would have
		// granted rather than a second, differently-derived rule.
		prefix, ok := permission.ApprovalBashPrefix(m.pendingApproval.Preview, command)
		if !ok {
			return fmt.Errorf("command cannot form a durable bash prefix")
		}
		// Store as "prefix *" when the approved command has extra args, so the
		// durable rule behaves like Claude's Bash(prefix *).
		if fields := strings.Fields(strings.TrimSpace(command)); len(fields) > len(strings.Fields(prefix)) {
			prefix = prefix + " *"
		}
		_, err := m.agent.AddWorkspaceBashPrefix(prefix)
		return err
	case permission.DurableMCPTool:
		_, err := m.agent.AddWorkspaceMCPTool(m.pendingApproval.ToolName)
		return err
	case permission.DurableWritePath:
		path := m.pendingApproval.Preview.Path
		if path == "" {
			path, _ = m.pendingApproval.Args["path"].(string)
		}
		if strings.TrimSpace(path) == "" {
			return fmt.Errorf("approval has no path to save")
		}
		_, err := m.agent.AddWorkspaceWritePath(path)
		return err
	default:
		return nil
	}
}

// navigateApprovalViewport leaves arrows and j/k to the focused decision rows.
// Long commands, diffs, and exact arguments retain explicit paging controls.
func (m *Model) navigateApprovalViewport(keyName string) bool {
	if m.approvalState == nil {
		return false
	}
	switch keyName {
	case "pgdown":
		m.approvalState.Viewport.PageDown()
	case "pgup":
		m.approvalState.Viewport.PageUp()
	case "ctrl+d":
		m.approvalState.Viewport.HalfPageDown()
	case "ctrl+u":
		m.approvalState.Viewport.HalfPageUp()
	case "home":
		m.approvalState.Viewport.GotoTop()
	case "end":
		m.approvalState.Viewport.GotoBottom()
	default:
		return false
	}
	return true
}

func (m *Model) resizeApproval(preserveOffset bool) {
	if m.approvalState == nil || m.pendingApproval == nil {
		return
	}
	offset := 0
	if preserveOffset {
		offset = m.approvalState.Viewport.YOffset()
	}
	width := m.approvalContentWidth()
	content := m.buildApprovalContent(width)
	bodyHeight := min(max(1, lipgloss.Height(content)), m.approvalBodyHeight())
	vp := viewport.New(
		viewport.WithWidth(width),
		viewport.WithHeight(bodyHeight),
	)
	// The smart parent owns a small, consistent read-only navigation grammar.
	vp.KeyMap.Up = key.NewBinding(key.WithDisabled())
	vp.KeyMap.Down = key.NewBinding(key.WithDisabled())
	vp.KeyMap.PageUp = key.NewBinding(key.WithDisabled())
	vp.KeyMap.PageDown = key.NewBinding(key.WithDisabled())
	vp.KeyMap.HalfPageUp = key.NewBinding(key.WithDisabled())
	vp.KeyMap.HalfPageDown = key.NewBinding(key.WithDisabled())
	vp.SetContent(content)
	vp.SetYOffset(offset)
	m.approvalState.Viewport = vp
}

func (m *Model) renderApproval() string {
	if m.approvalState == nil || m.pendingApproval == nil {
		return ""
	}
	grid := m.contentGrid()
	contentWidth := grid.ContentWidth()
	toolName := boundedApprovalMetadata(m.pendingApproval.ToolName, approvalMaximumActionBytes)
	if toolName == "" {
		toolName = "unknown tool"
	}
	mode := m.modeConfigs[m.presentedMode()].Label
	// Keep the tool identity ahead of the mode so narrow terminals preserve the
	// action being authorized. The authority mode remains explicit whenever the
	// available width permits it.
	// Permission glyph lives in the content-grid accent cell so the title text
	// shares OriginX=3 with tools; body lines use a space accent.
	permissionGlyph := "◇"
	if m.glyphProfile == GlyphASCII {
		permissionGlyph = "?"
	}
	separator := glyphSeparator(m.glyphProfile)
	titleText := approvalTitleText(toolName, mode, separator, contentWidth, m.glyphProfile)
	title := grid.Line(
		permissionGlyph,
		m.styles.ApprovalPrompt.Render(titleText),
	)
	var sections []string
	if saved := m.renderApprovalComposerReceipt(contentWidth); saved != "" {
		sections = append(sections, saved)
	}
	sections = append(sections, m.approvalState.Viewport.View(), m.renderApprovalChoices(contentWidth))
	detailAction := "arguments"
	if m.approvalState.ShowArguments {
		detailAction = "preview"
	}
	hints := m.renderKeyHints(contentWidth,
		keyHint{Key: "esc", Action: "cancel turn"},
		keyHint{Key: "enter", Action: "select"},
		keyHint{Key: "↑/↓/j/k", Action: "move"},
		keyHint{Key: "d", Action: detailAction},
		keyHint{Key: "pgup/dn", Action: "scroll"},
	)
	if contentWidth < 40 {
		hints = m.renderKeyHints(contentWidth,
			keyHint{Key: "esc", Action: "cancel turn"},
			keyHint{Key: "enter", Action: "select"},
		)
		detailHints := []keyHint{{Key: "d", Action: detailAction}}
		switch {
		case !m.approvalState.Viewport.AtBottom():
			detailHints = append([]keyHint{{Key: "pgdn", Action: "more"}}, detailHints...)
		case m.approvalState.Viewport.YOffset() > 0:
			detailHints = append([]keyHint{{Key: "pgup", Action: "previous"}}, detailHints...)
		}
		hints += "\n" + m.renderKeyHints(contentWidth, detailHints...)
	}
	sections = append(sections, hints)
	body := indentApprovalSurface(strings.Join(sections, "\n"), grid.PaneWidth, m.glyphProfile)
	if body == "" {
		return title
	}
	return title + "\n" + body
}

func (m *Model) renderApprovalComposerReceipt(width int) string {
	hasDraft := m.input.Value() != ""
	hasQueue := m.queuedFollowUp != nil
	if !hasDraft && !hasQueue {
		return ""
	}

	var candidates []string
	switch {
	case hasDraft && hasQueue:
		candidates = []string{"Draft saved · queued follow-up saved", "Draft + queue saved", "Input saved"}
	case hasDraft:
		candidates = []string{"Composer draft saved", "Draft saved", "Input saved"}
	default:
		candidates = []string{"Queued follow-up saved", "Queue saved", "Input saved"}
	}
	chosen := candidates[len(candidates)-1]
	for _, candidate := range candidates {
		if lipgloss.Width(candidate) <= width {
			chosen = candidate
			break
		}
	}
	if m.glyphProfile == GlyphASCII {
		chosen = strings.ReplaceAll(chosen, " · ", glyphSeparator(GlyphASCII))
	}
	return m.styles.OverlayDim.Render(
		truncateDisplayWithGlyphProfile(chosen, max(1, width), m.glyphProfile),
	)
}

func (m *Model) renderApprovalChoices(width int) string {
	if m.approvalState == nil || width <= 0 {
		return ""
	}
	choices := m.currentApprovalChoices()
	if len(choices) == 0 {
		return ""
	}
	selected := m.approvalState.ChoiceIndex
	if selected < 0 || selected >= len(choices) {
		selected = 0
	}

	choiceView := func(choice approvalChoice, active, compact bool) string {
		indicator := "  "
		label := choice.Label
		if compact {
			label = choice.CompactLabel
		}
		if m.glyphProfile == GlyphASCII {
			label = strings.ReplaceAll(label, " · ", glyphSeparator(GlyphASCII))
		}
		if active {
			indicatorGlyph := "›"
			if m.glyphProfile == GlyphASCII {
				indicatorGlyph = glyphSet(GlyphASCII).Right
			}
			indicator = m.styles.FocusIndicator.Render(indicatorGlyph + " ")
		}
		keyView := m.styles.FocusIndicator.Render(choice.Key)
		labelView := m.styles.OverlayDim.Render(label)
		return indicator + keyView + " " + labelView
	}

	// choiceRow degrades per row rather than per screen. Once the choices stack
	// vertically each row owns the full width, so the compact label is only
	// warranted when the prose one genuinely does not fit. Reaching for it as
	// soon as the single-row layout failed is why an 89-column terminal asked
	// the user to choose between "same/session" and "tool/session" — two labels
	// that do not say what they do — while the prose that explains them
	// ("same request again this session") was already written and unused.
	choiceRow := func(choice approvalChoice, active bool, budget int) string {
		if full := choiceView(choice, active, false); lipgloss.Width(full) <= budget {
			return full
		}
		compact := choiceView(choice, active, true)
		if lipgloss.Width(compact) <= budget {
			return compact
		}
		return truncateDisplayWithGlyphProfile(compact, budget, m.glyphProfile)
	}

	wide := make([]string, 0, len(choices)*2-1)
	for index, choice := range choices {
		if index > 0 {
			wide = append(wide, m.styles.OverlayDim.Render(glyphSeparator(m.glyphProfile)))
		}
		wide = append(wide, choiceView(choice, index == selected, false))
	}
	wideView := lipgloss.JoinHorizontal(lipgloss.Top, wide...)
	if lipgloss.Width(wideView) <= width {
		return wideView
	}

	// One choice per row keeps minimum-width screens deterministic across
	// platforms. Pairing two compact choices per line truncates differently
	// depending on terminal width accounting for focus glyphs.
	rows := make([]string, 0, len(choices))
	for index, choice := range choices {
		row := choiceRow(choice, index == selected, width)
		if width < 40 {
			row = strings.TrimLeft(row, " ")
		}
		rows = append(rows, row)
	}
	return lipgloss.JoinVertical(lipgloss.Left, rows...)
}

// approvalContentWidth is the content-grid flex column used by approval
// commands, diffs, and choice rows. It matches tools/transcript WorkWidth
// (pane − left chrome − right chrome) so painted lines share OriginX=3.
func (m *Model) approvalContentWidth() int {
	return m.contentGrid().ContentWidth()
}

// approvalTitleText prefers the tool identity over the mode label when the
// content-grid flex column is tight. Mode is dropped before the tool name is
// truncated so minimum terminals still show which action is being authorized.
func approvalTitleText(toolName, mode, separator string, width int, profile GlyphProfile) string {
	base := "Permission" + separator + toolName
	withMode := base + separator + mode
	switch {
	case lipgloss.Width(withMode) <= width:
		return withMode
	case lipgloss.Width(base) <= width:
		return base
	default:
		return truncateDisplayWithGlyphProfile(base, width, profile)
	}
}

// approvalBodyHeight keeps enough transcript visible to preserve context while
// allowing consequential requests to expose useful detail. The Bubbles
// viewport owns overflow, so minimum terminals retain the action, target, and
// complete decision grammar without pushing the surface off-screen.
func (m *Model) approvalBodyHeight() int {
	switch {
	case m.height <= 12:
		return 2
	case m.height < 20:
		return 4
	case m.height < 30:
		return 8
	default:
		return 12
	}
}

// indentApprovalSurface aligns decision-surface lines to the transcript
// content grid (accent + pad → OriginX=3). Each non-empty line is truncated to
// ContentWidth before the grid prefix is applied so overflow never past
// LineWidth. Empty lines stay empty for vertical rhythm.
func indentApprovalSurface(value string, paneWidth int, profiles ...GlyphProfile) string {
	profile := resolveGlyphProfile(profiles...)
	grid := ContentGrid{PaneWidth: max(1, paneWidth), Profile: profile}
	if value == "" {
		return ""
	}
	lines := strings.Split(value, "\n")
	for index, line := range lines {
		if line == "" {
			continue
		}
		lines[index] = grid.Line(" ", line)
	}
	return strings.Join(lines, "\n")
}

func (m *Model) buildApprovalContent(width int) string {
	if m.pendingApproval == nil {
		return ""
	}
	if m.approvalState != nil && m.approvalState.ShowArguments {
		return m.buildApprovalArguments(width)
	}
	return m.buildApprovalPreview(width)
}

func (m *Model) buildApprovalPreview(width int) string {
	request := m.pendingApproval
	preview := request.Preview
	var lines []string

	appendRow := func(label, value string) {
		value = sanitizeApprovalMetadata(value)
		if value == "" {
			return
		}
		labelWidth := min(10, max(6, width/5))
		available := max(1, width-labelWidth-1)
		wrapped := strings.Split(wrapText(value, available), "\n")
		lines = append(lines, m.styles.OverlayAccent.Width(labelWidth).Render(label)+" "+wrapped[0])
		for _, continuation := range wrapped[1:] {
			lines = append(lines, strings.Repeat(" ", labelWidth+1)+continuation)
		}
	}
	appendPathRow := func(label, value string) {
		value = sanitizeApprovalMetadata(value)
		if value == "" {
			return
		}
		if m.approvalBodyHeight() <= 2 {
			// Min-height viewport is only two rows (Action + path). Drop the
			// label so the full content-grid flex column can hold basenames
			// like approval-probe.txt under OriginX=3 chrome.
			value = compactWorkspacePath(value, width)
			lines = append(lines, truncateDisplayWithGlyphProfile(value, width, m.glyphProfile))
			return
		}
		appendRow(label, value)
	}

	actionLabel := boundedApprovalMetadata(preview.ActionLabel, approvalMaximumActionBytes)
	hasCustomAction := actionLabel != ""
	if !hasCustomAction {
		actionLabel = boundedApprovalMetadata(request.ToolName, approvalMaximumActionBytes)
	}
	switch preview.Kind {
	case permission.PreviewFileWrite:
		if !hasCustomAction {
			actionLabel = "Write " + formatApprovalBytes(preview.ByteSize)
		}
		appendRow("Action", actionLabel)
		appendPathRow("Target", preview.Path)
	case permission.PreviewFilePatch:
		if !hasCustomAction {
			actionLabel = "Patch file"
		}
		appendRow("Action", actionLabel)
		appendPathRow("Target", preview.Path)
	case permission.PreviewCommand:
		if !hasCustomAction {
			actionLabel = "Run command"
		}
		appendRow("Action", actionLabel)
	case permission.PreviewFilesystem:
		if !hasCustomAction {
			actionLabel = "Change filesystem"
		}
		appendRow("Action", actionLabel)
		appendPathRow("Path", preview.Path)
		appendPathRow("From", preview.SourcePath)
		appendPathRow("To", preview.DestinationPath)
	default:
		appendRow("Action", "Run "+actionLabel)
		appendPathRow("Target", preview.Path)
	}
	// Reason is host-authored static policy text (for example which scoped-shell
	// rule a bash command tripped). It answers "why is this being asked" ahead
	// of the generic consequence and the exact command.
	if preview.Reason != "" {
		appendRow("Reason", preview.Reason)
	}
	appendRow("Impact", boundedApprovalMetadata(preview.Consequence, approvalMaximumConsequenceBytes))
	// Progressive disclosure: Scope stays calm on narrow panes and is visible by
	// default on wide surfaces (>= 40 content columns).
	//
	// The argument digest is deliberately not here. It is an opaque hex handle
	// that nobody decides anything from, and buildApprovalArguments already
	// prints it as "Bound to <digest>" beside the exact arguments it fingerprints
	// — which is the only place it means anything. The d/ShowArguments toggle
	// reaches it without changing permission keys or decision semantics.
	if width >= 40 {
		appendRow("Scope", approvalScopeLabel(request.Scope))
	}

	switch preview.Kind {
	case permission.PreviewCommand:
		lines = append(lines, "", m.styles.OverlayAccent.Render("Command"))
		lines = append(lines, approvalWrappedLines(preview.Command, width)...)
	case permission.PreviewFileWrite, permission.PreviewFilePatch:
		lines = append(lines, "", m.styles.OverlayAccent.Render("Proposed change"))
		switch {
		case preview.Diff != "":
			lines = append(lines, m.renderApprovalDiff(preview.Diff, width)...)
			if preview.DiffTruncated {
				lines = append(lines, m.styles.OverlayDim.Render("Diff preview truncated; press d for exact arguments."))
			}
		case preview.DiffOmittedReason != "":
			lines = append(lines, m.styles.OverlayDim.Render(wrapText(preview.DiffOmittedReason, width)))
			lines = append(lines, m.styles.OverlayDim.Render("Press d to inspect the exact arguments."))
		default:
			lines = append(lines, m.styles.OverlayDim.Render("No textual difference detected."))
		}
	case permission.PreviewGeneric:
		lines = append(lines, "", m.styles.OverlayDim.Render("Press d to inspect the exact arguments."))
	}

	return strings.Join(lines, "\n")
}

func boundedApprovalMetadata(value string, maximumBytes int) string {
	value = sanitizeApprovalMetadata(value)
	if maximumBytes <= 0 || value == "" {
		return ""
	}
	if len(value) <= maximumBytes {
		return value
	}
	marker := "..."
	limit := maximumBytes - len(marker)
	if limit <= 0 {
		return marker[:maximumBytes]
	}
	for limit > 0 && !utf8.ValidString(value[:limit]) {
		limit--
	}
	return strings.TrimSpace(value[:limit]) + marker
}

// sanitizeApprovalMetadata treats every label supplied by a model or MCP
// server as untrusted terminal data. Exact arguments remain available in the
// JSON details view; presentation metadata must never be able to emit ANSI,
// OSC, C0/C1, newline, tab, or bidi reordering controls into the decision UI.
func sanitizeApprovalMetadata(value string) string {
	value = sanitizeApprovalPreviewLine(value)
	return strings.Join(strings.Fields(value), " ")
}

// sanitizeApprovalPreviewLine preserves ordinary spacing in commands, diffs,
// and JSON while removing sequences that can change terminal state or visual
// ordering. Callers pass one logical line at a time.
func sanitizeApprovalPreviewLine(value string) string {
	value = sanitizeTerminalMultiline(value)
	return strings.NewReplacer("\t", "    ", "\n", " ").Replace(value)
}

func (m *Model) buildApprovalArguments(width int) string {
	encoded, err := json.MarshalIndent(m.pendingApproval.Args, "", "  ")
	if err != nil {
		return m.styles.OverlayDim.Render("Exact arguments unavailable: " + err.Error())
	}
	lines := []string{
		m.styles.OverlayAccent.Render("Exact arguments"),
		m.styles.OverlayDim.Render("Bound to " + shortApprovalDigest(m.pendingApproval.ArgumentsSHA256)),
		"",
	}
	for _, line := range strings.Split(string(encoded), "\n") {
		wrapped := approvalWrappedLines(line, width)
		if len(wrapped) == 0 {
			lines = append(lines, "")
			continue
		}
		lines = append(lines, wrapped...)
	}
	return strings.Join(lines, "\n")
}

func (m *Model) renderApprovalDiff(diff string, width int) []string {
	rawLines := strings.Split(diff, "\n")
	lines := make([]string, 0, len(rawLines))
	for index, line := range rawLines {
		// The line diff for a new file can begin with a synthetic deletion of
		// the empty preimage ("-"). It carries no reviewable material and can
		// consume the last visible preview row, hiding the actual addition.
		if line == "-" && index+1 < len(rawLines) && strings.HasPrefix(rawLines[index+1], "+") {
			continue
		}
		style := m.styles.DiffContext.PaddingLeft(0)
		switch {
		case strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++"):
			style = m.styles.DiffAdded.PaddingLeft(0)
		case strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---"):
			style = m.styles.DiffRemoved.PaddingLeft(0)
		case strings.HasPrefix(line, "@@"), strings.HasPrefix(line, "---"), strings.HasPrefix(line, "+++"):
			style = m.styles.DiffHeader.PaddingLeft(0)
		}
		wrapped := approvalWrappedLines(line, width)
		if len(wrapped) == 0 {
			lines = append(lines, "")
			continue
		}
		for _, segment := range wrapped {
			lines = append(lines, style.Render(segment))
		}
	}
	return lines
}

func approvalWrappedLines(value string, width int) []string {
	value = sanitizeApprovalPreviewLine(value)
	if value == "" {
		return nil
	}
	return strings.Split(wrapText(value, max(1, width)), "\n")
}

func approvalScopeLabel(scope permission.ApprovalScope) string {
	switch scope.Kind {
	case permission.ScopeExactRequest:
		return "Exact arguments · this process only"
	case permission.ScopeSessionTool:
		return "This tool · any args · this process"
	case permission.ScopeSessionPath:
		if resource := strings.TrimSpace(scope.Resource); resource != "" {
			return "Path " + compactApprovalHint(resource, 40) + " · write/edit/mkdir · this process"
		}
		return "This path · write/edit/mkdir · this process"
	case permission.ScopeSessionBashPrefix:
		if resource := strings.TrimSpace(scope.Resource); resource != "" {
			return "Bash pattern " + compactApprovalHint(resource, 32) + " · this process"
		}
		return "Bash prefix · this process"
	case permission.ScopeSessionMCPTool:
		return "This MCP tool · any args · this process"
	default:
		return "This request only"
	}
}

func shortApprovalDigest(digest string) string {
	digest = strings.TrimSpace(digest)
	if digest == "" {
		return "unavailable"
	}
	if len(digest) > 12 {
		return digest[:12]
	}
	return digest
}

func formatApprovalBytes(size int64) string {
	if size < 1024 {
		return fmt.Sprintf("%d B", size)
	}
	if size < 1024*1024 {
		return fmt.Sprintf("%.1f KiB", float64(size)/1024)
	}
	return fmt.Sprintf("%.1f MiB", float64(size)/(1024*1024))
}

// handleToolApprovalRequest opens the interactive approval prompt, or settles
// the request as host cancellation when the application is shutting down.
func (m *Model) handleToolApprovalRequest(msg ToolApprovalMsg) {
	if m.shuttingDown {
		// A callback may cross the shutdown boundary after the active turn has
		// already been cancelled. Never reopen interactive authority while the
		// host is joining receipts, and never mislabel host cancellation as a
		// human denial. Production response channels are buffered; the
		// non-blocking send also keeps malformed or already-settled adapters from
		// freezing the Bubble Tea parent during shutdown.
		if msg.Response != nil {
			select {
			case msg.Response <- permission.Cancelled("application is shutting down"):
			default:
			}
		}
		return
	}
	if err := m.openApproval(msg); err != nil {
		if msg.Response != nil {
			msg.Response <- permission.Refuse("approval_preview_unavailable", err.Error())
		}
		m.entries = append(m.entries, ChatEntry{
			Kind:    "error",
			Content: "Approval preview unavailable: " + err.Error(),
		})
		m.invalidateRenderedCache()
		m.refreshTranscript()
		m.gotoBottomIfFollowing()
	}
}
