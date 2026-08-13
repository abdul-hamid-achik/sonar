package ui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
)

// renderStatusLine builds the status bar above the input/hint area.
func (m *Model) renderStatusLine() string {
	paneW := m.chatPaneWidth()
	// Approval owns the complete inline composer surface and is rendered by
	// View, so the ordinary status line stays quiet behind it.
	if m.pendingApproval != nil {
		return ""
	}
	if m.readScopePrompt != nil {
		return ""
	}
	// Completion uses the full composer-owned footer budget for the popup and
	// the still-visible draft; its own key footer carries the active guidance.
	if m.overlay == OverlayCompletion && m.isCompletionActive() {
		return ""
	}
	// Search owns a fixed footer surface with its own result count and keys.
	if m.overlay == OverlayTranscriptSearch && m.transcriptSearch != nil {
		return ""
	}
	// The Cortex decision frame carries its own state and key guidance, including
	// in-flight liveness, so no competing working/status footer is rendered.
	if m.cortexDecisionActive() {
		return ""
	}
	// Structured Plan and Goal editing own the same footer region. Their field
	// labels and key footer are the complete active status while open.
	if m.inlineFormActive() {
		return ""
	}

	// Pending paste prompt overrides normal status.
	if m.pendingPaste != nil {
		pending := m.pendingPaste
		switch {
		case !pending.PlainFits:
			return m.renderDecisionPrompt(
				"Paste too large", pending.descriptor(),
				keyHint{Key: "esc", Action: "dismiss"},
				keyHint{Action: "use @file or /load"},
			)
		case !pending.FencedFits:
			return m.renderDecisionPrompt(
				"Large paste", pending.descriptor()+" · plain only",
				keyHint{Key: "esc", Action: "cancel"},
				keyHint{Key: "y", Action: "plain"},
			)
		default:
			return m.renderDecisionPrompt(
				"Large paste", pending.descriptor(),
				keyHint{Key: "esc", Action: "cancel"},
				keyHint{Key: "y", Action: "code"},
				keyHint{Key: "n", Action: "plain"},
			)
		}
	}
	if m.pendingSessionSwitch != nil && m.pendingSessionSwitch.Choice == sessionSwitchUndecided {
		return m.renderSessionSwitchPrompt(paneW)
	}
	if m.followPaused() && m.state == StateIdle && !m.composerIsBusy() {
		return m.renderFollowPausedStatus(paneW)
	}
	if m.state != StateIdle || m.composerIsBusy() {
		return m.renderWorkingLine()
	}
	if m.standaloneRecovery != nil {
		titleLimit := 0
		if paneW >= 72 {
			titleLimit = 24
		}
		return m.renderDecisionPrompt(
			"Recovery paused", sessionDisplayLabel(m.sessionPublicID, m.activeSessionTitle, titleLimit),
			keyHint{Key: "/recover", Action: "inspect"},
		)
	}
	if len(m.pendingImages) > 0 {
		return m.renderPendingImagesStatus(paneW)
	}
	if summary, ok := m.goalStatusSummary(); ok {
		return m.renderGoalFooterStatus(summary, paneW)
	}
	conversationStarted := m.conversationStarted()
	hasNotice := m.hasTranscriptNotice()
	noticeNeedsRecovery := hasNotice && (paneW < 36 || m.height < 16)

	// Empty welcome + session top bar own orientation and ambient identity.
	// Repeating model/context on the idle footer only adds a sparse second row.
	conversationQuiet := !conversationStarted && !noticeNeedsRecovery &&
		len(m.failedServers) == 0 && !m.skipApprovalsEnabled() && m.footerNotice == nil &&
		m.mouseCaptureOff
	if conversationQuiet {
		return ""
	}

	presentedMode := m.presentedMode()
	cfg := m.modeConfigs[presentedMode]
	var modeStyle lipgloss.Style
	switch presentedMode {
	case ModeNormal:
		modeStyle = m.styles.ModeAsk
	case ModePlan:
		modeStyle = m.styles.ModePlan
	case ModeAuto:
		modeStyle = m.styles.ModeBuild
	}
	modeLabel := cfg.Label
	if paneW >= 40 {
		modeLabel = "[ " + modeLabel + " ]"
	}
	parts := make([]string, 0, 8)
	plan := m.planStatus()
	if plan.owns(factMode, surfaceStatusLine) && presentedMode != ModeNormal {
		if presentedMode == ModePlan && paneW >= 48 {
			parts = append(parts, modeStyle.Render("[ PLAN · read-only ]"))
		} else {
			parts = append(parts, modeStyle.Render(modeLabel))
		}
	}
	if m.skipApprovalsEnabled() {
		parts = append(parts, m.styles.StatusWarning.Render("approval prompts skipped"))
	} else if m.acceptWorkspaceEditsEnabled() {
		parts = append(parts, m.styles.StatusWarning.Render("accept workspace edits"))
	} else if conversationStarted && paneW >= 58 {
		// Positive confirmation that the host still gates mutation. Welcome
		// already states posture on empty frames; keep this once work starts.
		parts = append(parts, m.styles.StatusText.Render("approvals on"))
	}
	if !conversationStarted && noticeNeedsRecovery {
		// Startup and recovery notices can push the empty-state hints out of a
		// minimum-height viewport. Keep the Settings recovery path in the fixed
		// footer until a real conversation begins.
		parts = append(parts, m.styles.FocusIndicator.Render("ctrl+p settings"))
	}

	if failures := len(m.failedServers); failures > 0 {
		label := mcpUnavailableStatusLabel(failures)
		parts = append(parts, m.styles.ErrorText.UnsetPaddingLeft().Render(label))
	}
	if !m.mouseCaptureOff {
		// Sticky until alt+m hands the mouse back. A footerNotice expires in
		// seconds and left people thinking drag-select was still impossible.
		parts = append(parts, m.styles.FocusIndicator.Render("wheel · alt+m"))
	}
	if notice := m.footerNotice; notice != nil {
		parts = append(parts, m.footerNoticeStyle(notice.severity).Render(notice.text))
		if receiptAction, ok := m.inspectableToolReceiptAction(); notice.severity == noticeSuccess &&
			paneW >= 58 && strings.TrimSpace(m.input.Value()) == "" && ok {
			parts = append(parts,
				m.styles.FocusIndicator.Render(m.keys.ToggleFocusedTool.Help().Key)+
					" "+m.styles.StatusText.Render(receiptAction),
			)
		}
	}
	if session := sessionDisplayLabel(m.sessionPublicID, m.activeSessionTitle, sessionStatusTitleLimit(paneW)); session != "" {
		parts = append(parts, m.styles.StatusText.Render(session))
	}

	// Ambient identity is painted here only when this frame assigned it here.
	// The one exception is context pressure: a context window about to force a
	// compaction is an operational event, not ambient state, so it is promoted
	// to the front of this row regardless of who owns the resting meter.
	contextStatus := m.renderContextStatus()
	contextHigh := m.contextPressureHigh()
	if contextHigh && contextStatus != "" && plan.owns(factContext, surfaceStatusLine) {
		parts = append(parts, contextStatus)
	}
	if m.currentModelIsNonLocal() {
		if plan.owns(factRemoteBoundary, surfaceStatusLine) {
			if model := m.currentModelReachabilityLabel(paneW < 58); model != "" {
				parts = append(parts, m.styles.StatusWarning.Render(model))
			}
		}
	} else if plan.owns(factModel, surfaceStatusLine) {
		if model := m.currentModelReachabilityLabel(paneW < 58); model != "" {
			parts = append(parts, m.styles.StatusText.Render(model))
		}
	}
	if profile := sanitizeTerminalSingleLine(m.agentProfile); paneW >= 80 && profile != "" &&
		plan.ownedBy(factModel) == surfaceStatusLine {
		parts = append(parts, m.styles.StatusText.Render("@"+profile))
	}
	if !contextHigh && contextStatus != "" && plan.owns(factContext, surfaceStatusLine) {
		parts = append(parts, contextStatus)
	}
	// The chip answers "will this speak" from across the room. Last in the
	// row on purpose: the width-trim below drops from the right, and every
	// operational signal above outranks an ambient reminder.
	if plan.owns(factVoice, surfaceStatusLine) {
		parts = append(parts, m.styles.StatusText.Render("voice on"))
	}
	// Shortcuts bar already carries enter/mode/help. Idle footer only adds
	// operational signals (mode, approvals, MCP, notices, high context) —
	// not a second discoverability strip that fights the fixed chrome.
	if len(parts) == 0 {
		return ""
	}

	separator := m.styles.StatusText.Render(glyphSeparator(m.glyphProfile))
	// Match transcript content-grid OriginX so status text starts at column 3.
	lead := m.contentGrid().Prefix(" ")
	line := lead + strings.Join(parts, separator)
	// Drop optional metadata from the right. Mode and operational failure are
	// first, so they survive every supported width tier.
	for lipgloss.Width(line) > paneW && len(parts) > 2 {
		parts = parts[:len(parts)-1]
		line = lead + strings.Join(parts, separator)
	}
	if lipgloss.Width(line) > paneW {
		// Preserve every compact safety boundary instead of truncating a single
		// concatenated string from the right. Combined Cloud, MCP, and approval
		// posture can legitimately need a second row at the minimum width.
		compact := make([]string, 0, 4)
		if presentedMode != ModeNormal && plan.owns(factMode, surfaceStatusLine) {
			compact = append(compact, modeStyle.Render(cfg.Label))
		}
		if m.skipApprovalsEnabled() {
			compact = append(compact, m.styles.StatusWarning.Render("no prompts"))
		} else if m.acceptWorkspaceEditsEnabled() {
			compact = append(compact, m.styles.StatusWarning.Render("accept edits"))
		}
		if len(m.failedServers) > 0 {
			compact = append(compact, m.styles.ErrorText.UnsetPaddingLeft().Render("MCP unavailable"))
		}
		if m.currentModelIsNonLocal() && plan.owns(factRemoteBoundary, surfaceStatusLine) {
			boundary := strings.Fields(m.currentModelSurfaceLabel(true))[0]
			compact = append(compact, m.styles.StatusWarning.Render(boundary))
		}
		if session := sessionDisplayLabel(m.sessionPublicID, "", 0); session != "" {
			compact = append(compact, m.styles.StatusText.Render(session))
		}
		if len(compact) == 0 {
			// Ambient-only rows (model · context) still deserve a single
			// truncated line on minimum widths instead of vanishing.
			return truncateDisplayWithGlyphProfile(line, paneW, m.glyphProfile)
		}
		return renderPackedStatusRows(paneW, compact, separator)
	}
	return line
}

func sessionStatusTitleLimit(paneW int) int {
	if paneW < 72 {
		return 0
	}
	return min(32, max(16, paneW/3))
}

// renderGoalFooterStatus keeps Goal Runtime additive: progress joins the
// normal mode/model/context grammar instead of replacing it. Optional metadata
// yields from the right while mode and a useful goal label survive every
// supported width tier.
func (m *Model) renderGoalFooterStatus(summary GoalSummary, paneW int) string {
	if paneW <= 1 {
		return ""
	}
	// This row replaced renderStatusLine wholesale and re-derived every ambient
	// fact from scratch, so an attached goal reprinted the model, the meter and
	// the Cloud boundary one line under the top bar that already owned them.
	plan := m.planStatus()
	available := paneW - 1 // preserve the status row's leading breathing cell
	// An attached Goal Runtime always dispatches with AUTO authority. m.mode is
	// only the ambient selection for future non-goal turns, so it must not tint
	// or label this active-goal status row.
	cfg := m.modeConfigs[ModeAuto]
	modeStyle := m.styles.ModeBuild
	modeLabel := cfg.Label
	if paneW >= 48 {
		modeLabel = "[ " + modeLabel + " ]"
	}
	modePart := modeStyle.Render(modeLabel)
	separator := m.styles.StatusText.Render(glyphSeparator(m.glyphProfile))

	type metadataPart struct {
		view string
	}
	required := make([]metadataPart, 0, 3)
	if m.skipApprovalsEnabled() {
		label := "approval prompts skipped"
		if paneW < 58 {
			label = "no prompts"
		}
		required = append(required, metadataPart{view: m.styles.StatusWarning.Render(label)})
	} else if m.acceptWorkspaceEditsEnabled() {
		label := "accept workspace edits"
		if paneW < 58 {
			label = "accept edits"
		}
		required = append(required, metadataPart{view: m.styles.StatusWarning.Render(label)})
	}
	contextStatus := m.renderContextStatus()
	contextHigh := m.contextPressureHigh()
	if failures := len(m.failedServers); failures > 0 {
		label := "MCP unavailable"
		if paneW >= 58 {
			label = mcpUnavailableStatusLabel(failures)
		}
		required = append(required, metadataPart{view: m.styles.ErrorText.UnsetPaddingLeft().Render(label)})
	}
	if !m.mouseCaptureOff {
		required = append(required, metadataPart{view: m.styles.FocusIndicator.Render("wheel · alt+m")})
	}
	if m.currentModelIsNonLocal() && plan.owns(factRemoteBoundary, surfaceStatusLine) {
		boundary := m.currentModelSurfaceLabel(true)
		if paneW < 58 {
			boundary = strings.Fields(boundary)[0]
		}
		required = append(required, metadataPart{view: m.styles.StatusWarning.Render(boundary)})
	}
	if session := sessionDisplayLabel(m.sessionPublicID, "", 0); session != "" {
		required = append(required, metadataPart{view: m.styles.StatusText.Render(session)})
	}

	optional := make([]metadataPart, 0, 5)
	// The goal label already names the work. At roomy widths add only the
	// compact durable handle; repeating the session title would squeeze the
	// goal phase, budget, and objective that matter more here.
	if contextHigh && contextStatus != "" && plan.owns(factContext, surfaceStatusLine) {
		optional = append(optional, metadataPart{view: contextStatus})
	}
	if model := m.currentModelReachabilityLabel(false); model != "" &&
		!m.currentModelIsNonLocal() && plan.owns(factModel, surfaceStatusLine) {
		optional = append(optional, metadataPart{view: m.styles.StatusText.Render(
			truncateDisplayWithGlyphProfile(model, 20, m.glyphProfile),
		)})
	}
	if !contextHigh && contextStatus != "" && plan.owns(factContext, surfaceStatusLine) {
		optional = append(optional, metadataPart{view: contextStatus})
	}
	if notice := m.footerNotice; notice != nil {
		optional = append(optional, metadataPart{view: m.footerNoticeStyle(notice.severity).Render(notice.text)})
	}

	const minimumGoalWidth = 12
	fixedWidth := lipgloss.Width(modePart)
	if modePart != "" {
		fixedWidth += lipgloss.Width(separator)
	}
	requiredWidth := 0
	for _, candidate := range required {
		requiredWidth += lipgloss.Width(separator) + lipgloss.Width(candidate.view)
	}
	if len(required) > 0 && available-fixedWidth-requiredWidth < minimumGoalWidth {
		goalWidth := max(1, available-fixedWidth)
		core := " " + strings.Join([]string{modePart, RenderGoalStatusLine(summary, goalWidth, m.isDark, m.themeID, m.glyphProfile)}, separator)
		safety := make([]string, 0, len(required))
		for _, candidate := range required {
			safety = append(safety, candidate.view)
		}
		return truncateDisplayWithGlyphProfile(core, paneW, m.glyphProfile) +
			"\n" + renderPackedStatusRows(paneW, safety, separator)
	}

	selected := make([]string, 0, len(required)+len(optional))
	for _, candidate := range required {
		selected = append(selected, candidate.view)
		fixedWidth += lipgloss.Width(separator) + lipgloss.Width(candidate.view)
	}
	for _, candidate := range optional {
		cost := lipgloss.Width(separator) + lipgloss.Width(candidate.view)
		if available-fixedWidth-cost < minimumGoalWidth {
			continue
		}
		selected = append(selected, candidate.view)
		fixedWidth += cost
	}
	goalWidth := max(1, available-fixedWidth)
	goalPart := RenderGoalStatusLine(summary, goalWidth, m.isDark, m.themeID, m.glyphProfile)
	parts := make([]string, 0, 2+len(selected))
	if modePart != "" {
		parts = append(parts, modePart)
	}
	parts = append(parts, goalPart)
	parts = append(parts, selected...)
	line := " " + strings.Join(parts, separator)
	return truncateDisplayWithGlyphProfile(line, paneW, m.glyphProfile)
}

// renderPackedStatusRows keeps short host-authored status tokens intact while
// packing them into the fewest width-safe rows. It is reserved for compact
// safety fallbacks where dropping a rightmost token would hide active authority
// or a remote-execution boundary. Leading pad matches content-grid OriginX.
func renderPackedStatusRows(width int, parts []string, separator string) string {
	if width <= 0 || len(parts) == 0 {
		return ""
	}
	lead := strings.Repeat(" ", contentLeftColumns)
	available := max(1, width-contentLeftColumns)
	rows := make([]string, 0, 2)
	current := ""
	for _, part := range parts {
		if strings.TrimSpace(part) == "" {
			continue
		}
		part = truncateDisplay(part, available)
		candidate := part
		if current != "" {
			candidate = current + separator + part
		}
		if current != "" && lipgloss.Width(candidate) > available {
			rows = append(rows, lead+current)
			current = part
			continue
		}
		current = candidate
	}
	if current != "" {
		rows = append(rows, lead+current)
	}
	return strings.Join(rows, "\n")
}

func mcpUnavailableStatusLabel(count int) string {
	return fmt.Sprintf("⚠ %d MCP %s unavailable", count, pluralizeServer(count))
}

func (m *Model) hasTranscriptNotice() bool {
	for _, entry := range m.entries {
		if entry.Kind == "system" || entry.Kind == "error" {
			return true
		}
	}
	return false
}
