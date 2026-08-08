package ui

import (
	"os"
	"path/filepath"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// renderNarrowTerminalView keeps tiny terminals recoverable.
func (m *Model) renderNarrowTerminalView(hint string) tea.View {
	titleText := "TERMINAL TOO SMALL"
	if m.width < minTerminalWidth && m.height >= minTerminalHeight {
		titleText = "TERMINAL TOO NARROW"
	} else if m.height < minTerminalHeight && m.width >= minTerminalWidth {
		titleText = "TERMINAL TOO SHORT"
	}
	return m.renderTerminalPauseView(
		titleText,
		hint,
		[]string{"Input paused · ctrl+c quit", "Paused · ctrl+c", "ctrl+c"},
		"resize terminal",
	)
}

func (m *Model) renderTerminalInputResumeView() tea.View {
	title := "INPUT PAUSED"
	hint := "Restoring input after resize; input received here is ignored."
	controls := []string{"Waiting for quiet · ctrl+c quit", "Waiting · ctrl+c", "ctrl+c"}
	switch m.terminalInputResumePhase {
	case terminalInputResumeAwaitGesture:
		hint = "Input is quiet · press enter to resume."
		controls = []string{"enter resume · ctrl+c quit", "enter · ctrl+c", "ctrl+c"}
	case terminalInputResumeConfirmationQuiet:
		hint = "Confirming the input boundary; input received here is ignored."
		controls = []string{"Resuming · ctrl+c quit", "Resuming · ctrl+c", "ctrl+c"}
	}
	return m.renderTerminalPauseView(
		title,
		hint,
		controls,
		"restoring input",
	)
}

func (m *Model) renderTerminalPauseView(titleText, hint string, controlCandidates []string, titleSuffix string) tea.View {
	terminalWidth := max(1, m.width)
	terminalHeight := max(1, m.height)
	contentW := max(1, terminalWidth-2)

	rows := []string{m.styles.OverlayTitle.Render(truncateDisplay(titleText, contentW))}
	if terminalHeight > 2 {
		hintRows := strings.Split(wrapText(hint, contentW), "\n")
		maximumHintRows := max(0, terminalHeight-2)
		if len(hintRows) > maximumHintRows {
			hintRows = hintRows[:maximumHintRows]
		}
		for _, row := range hintRows {
			rows = append(rows, m.styles.StatusText.Render(truncateDisplay(row, contentW)))
		}
	}
	if terminalHeight > 1 {
		controlHint := "ctrl+c"
		for _, candidate := range controlCandidates {
			controlHint = candidate
			if lipgloss.Width(candidate) <= contentW {
				break
			}
		}
		rows = append(rows, m.styles.FocusIndicator.Render(truncateDisplay(controlHint, contentW)))
	}
	if len(rows) > terminalHeight {
		rows = rows[:terminalHeight]
	}
	for index, row := range rows {
		rows[index] = lipgloss.PlaceHorizontal(terminalWidth, lipgloss.Center, row)
	}
	content := strings.Join(rows, "\n")
	top := (terminalHeight - len(rows)) / 2
	if top > 0 {
		content = strings.Repeat("\n", top) + content
	}

	v := tea.NewView(content)
	v.AltScreen = true
	v.WindowTitle = m.windowTitleBase() + " · " + titleSuffix
	m.applyViewTheme(&v)
	return v
}

// windowTitleBase names the tab. A conversation title identifies a tab far
// better than the product name repeated across every window, so it leads when
// one exists and the product name is the fallback.
//
// The title is model-generated from the user's own prompt, and terminal titles
// leak into window-manager history and terminal integrations — the same reason
// the workspace basename is used here instead of the full path. So it is
// sanitized to a single line and truncated, never passed through raw.
func (m *Model) windowTitleBase() string {
	const product = "SONAR"
	lead := product
	if m != nil {
		if title := windowTitleFromSessionTitle(m.activeSessionTitle); title != "" {
			lead = title
		}
	}
	workspace := ""
	if m != nil && m.agent != nil {
		workspace = strings.TrimSpace(m.agent.WorkDir())
	}
	if workspace == "" {
		workspace, _ = os.Getwd()
	}
	workspace = filepath.Clean(workspace)
	if workspace == "." || filepath.Dir(workspace) == workspace {
		return lead
	}
	name := sanitizeTerminalSingleLine(filepath.Base(workspace))
	if name == "" || name == "." || name == string(filepath.Separator) {
		return lead
	}
	return truncateDisplay(lead+" · "+name, 72)
}

// windowTitleFromSessionTitle renders a session title as tab text, or "" when
// nothing usable survives.
//
// Session titles are model-generated and arrive as prose, so they carry
// markdown: a title that was literally "```" reached a real tab. Fences,
// backticks, and leading list/heading punctuation are stripped before the
// existing single-line sanitization, and a title that is nothing but that
// punctuation yields "" so the caller falls back to the product name.
func windowTitleFromSessionTitle(raw string) string {
	title := strings.TrimSpace(raw)
	// Drop fenced-code framing first: a fence line contributes no words.
	for _, fence := range []string{"```", "~~~"} {
		title = strings.ReplaceAll(title, fence, " ")
	}
	title = strings.Map(func(r rune) rune {
		if r == '`' || r == '*' || r == '_' || r == '#' || r == '>' {
			return ' '
		}
		return r
	}, title)
	title = strings.Join(strings.Fields(title), " ")
	title = sanitizeTerminalSingleLine(strings.TrimSpace(title))
	if title == "" {
		return ""
	}
	return truncateDisplay(title, 48)
}

func (m *Model) renderCompletionModal() string {
	view, _ := m.renderCompletionModalView()
	return view
}

func (m *Model) renderCompletionModalView() (string, *tea.Cursor) {
	cs := m.completionState
	if cs == nil {
		return "", nil
	}
	contentW := pickerListWidth(m.width)
	filter := cs.Filter
	filter.SetVirtualCursor(false)
	filter.SetWidth(completionFilterInputWidth(m.width))
	popupRows := completionPopupHeight(m.height, m.inputLines)
	// Border rows plus the one-line key footer live outside the content body.
	contentRows := max(2, popupRows-3)
	showTitle := contentRows >= 3
	showDivider := contentRows >= 5
	fixedRows := 1 // filter
	if showTitle {
		fixedRows++
	}
	if showDivider {
		fixedRows++
	}
	remainingRows := max(1, contentRows-fixedRows)
	previewRows := 0
	if cs.Kind == "attachments" && remainingRows >= 2 {
		previewRows = min(6, max(1, (remainingRows+1)/2))
	}
	itemRows := max(1, remainingRows-previewRows)
	inlinePreview := cs.Kind == "attachments" && previewRows == 0

	var b strings.Builder

	if showTitle {
		var title string
		switch cs.Kind {
		case "command":
			title = "Commands"
		case "attachments":
			if inlinePreview {
				title = "Attach Files · Preview"
			} else {
				title = "Attach Files & Agents"
			}
		case "skills":
			title = "Skills"
		default:
			title = "Complete"
		}
		if cs.Kind == "attachments" && cs.CurrentPath != "" {
			title += " · " + sanitizeTerminalSingleLine(cs.CurrentPath) + "/"
		}
		if cs.Searching {
			title += " · searching…"
		}
		b.WriteString(m.styles.OverlayTitle.Render(truncateDisplay(title, contentW)))
		b.WriteString("\n")
	}

	filterY := strings.Count(b.String(), "\n")
	filterPrompt := completionFilterPromptForGlyphProfile(m.glyphProfile)
	b.WriteString(m.styles.FocusIndicator.Render(filterPrompt))
	filterX := lipgloss.Width(filterPrompt)
	b.WriteString(filter.View())
	b.WriteString("\n")
	filterCursor := offsetCursor(filter.Cursor(), filterX, filterY)

	if showDivider {
		b.WriteString(m.styles.Divider.Render(
			strings.Repeat(glyphSet(m.glyphProfile).Horizontal, contentW),
		))
		b.WriteString("\n")
	}

	items := cs.FilteredItems
	if len(items) == 0 {
		empty := "  (no matches)"
		if cs.Searching {
			empty = "  (searching…)"
		}
		b.WriteString(m.styles.CompletionCategory.Render(truncateDisplay(empty, contentW)))
		b.WriteString("\n")
		for row := 1; row < itemRows; row++ {
			b.WriteString(strings.Repeat(" ", contentW))
			b.WriteString("\n")
		}
	} else {
		start := 0
		if cs.Index >= itemRows {
			start = cs.Index - itemRows + 1
		}
		end := start + itemRows
		if end > len(items) {
			end = len(items)
			start = max(0, end-itemRows)
		}

		glyphs := glyphSet(m.glyphProfile)
		for i := start; i < end; i++ {
			item := items[i]
			displayLabel := sanitizeTerminalSingleLine(item.Label)
			displayCategory := sanitizeTerminalSingleLine(item.Category)
			displayDescription := sanitizeTerminalSingleLine(item.Description)
			prefix := "  "
			if i == cs.Index {
				prefix = m.styles.FocusIndicator.Render(glyphs.Collapsed + " ")
			}

			// Check if selected (for multi-select)
			selectedMark := ""
			if cs.Selected != nil {
				// Find original index
				for oi, orig := range cs.AllItems {
					if orig.Label == item.Label && orig.Insert == item.Insert {
						if cs.Selected[oi] {
							selectedMark = m.styles.FocusIndicator.Render(" " + glyphs.Success)
						}
						break
					}
				}
			}

			category := ""
			if cs.Kind == "attachments" {
				category = completionCategoryDisplay(displayCategory)
				if inlinePreview {
					category = compactCompletionPreviewState(cs.Preview, category)
				}
			}
			categoryWidth := 0
			if category != "" {
				// Right-aligned category column with a two-cell gutter.
				categoryWidth = lipgloss.Width(category) + 2
			}
			labelWidth := max(1, contentW-2-categoryWidth-lipgloss.Width(selectedMark))
			label := truncateDisplay(displayLabel, labelWidth)
			description := ""
			if cs.Kind == "command" && displayDescription != "" {
				remaining := labelWidth - lipgloss.Width(label)
				if remaining >= 6 {
					description = " · " + truncateDisplay(displayDescription, remaining-3)
				}
			}
			desc := m.styles.CompletionCategory.Render(description)

			row := prefix + label + desc + selectedMark
			if i == cs.Index {
				row = prefix + m.styles.FocusIndicator.Render(label) + desc + selectedMark
			}
			if category != "" {
				gap := max(1, contentW-lipgloss.Width(row)-lipgloss.Width(category))
				row += strings.Repeat(" ", gap) + m.styles.CompletionCategory.Render(category)
			}
			b.WriteString(row)
			b.WriteString("\n")
		}
		for row := end - start; row < itemRows; row++ {
			b.WriteString(strings.Repeat(" ", contentW))
			b.WriteString("\n")
		}
	}

	if previewRows > 0 {
		preview := m.renderCompletionPreview(contentW, previewRows)
		lines := strings.Split(preview, "\n")
		if preview == "" {
			lines = nil
		}
		for row := 0; row < previewRows; row++ {
			if row < len(lines) {
				b.WriteString(lines[row])
			} else {
				b.WriteString(strings.Repeat(" ", contentW))
			}
			b.WriteString("\n")
		}
	}

	// Footer hints use the same priority grammar as every other modal.
	hints := []keyHint{
		{Key: m.keys.Cancel.Help().Key, Action: "cancel"},
		{Key: m.keys.CompleteSelect.Help().Key, Action: "select"},
		{Key: "↑/↓", Action: "move"},
	}
	if cs.Kind == "attachments" && cs.CurrentPath != "" {
		hints = append(hints, keyHint{Key: "backspace", Action: "up"})
	}
	if cs.Selected != nil {
		hints = append(hints, keyHint{Key: m.keys.CompleteToggle.Help().Key, Action: "toggle"})
	}
	return m.renderPickerFrame(b.String(), m.renderKeyHints(contentW, hints...)), pickerFrameCursor(filterCursor)
}

// completionCategoryDisplay keeps machine category tokens out of the UI.
func completionCategoryDisplay(category string) string {
	if category == "search_result" {
		return "search"
	}
	return category
}

// compactCompletionPreviewState keeps the selected item's preview state
// visible when the minimum-height popup has no dedicated preview row.
func compactCompletionPreviewState(preview completionPreview, fallback string) string {
	switch preview.State {
	case completionPreviewLoading:
		return "loading"
	case completionPreviewReady:
		return formatCompletionPreviewBytes(preview.Size)
	case completionPreviewBinary:
		return "binary"
	case completionPreviewError:
		return "error"
	case completionPreviewFolder:
		return "folder"
	case completionPreviewAgent:
		return "agent"
	default:
		return fallback
	}
}

func completionPopupHeight(terminalHeight, inputLines int) int {
	if terminalHeight <= 0 {
		terminalHeight = 24
	}
	inputLines = max(1, inputLines)
	// Reserve the terminal safety row, the complete composer, and the
	// transcript's four-row reading floor. Popups of six rows or fewer own
	// their top boundary and omit the redundant shared divider; roomier popups
	// reserve that divider explicitly.
	available := terminalHeight - 1 - inputLines - minTranscriptRows
	if available > 6 {
		available--
	}
	return min(15, max(5, available))
}

// The compact popup's top border is already a transcript/composer boundary.
// Omitting the redundant rule preserves one additional transcript row without
// hiding draft lines or completion controls.
func (m *Model) compactCompletionOwnsDivider() bool {
	return m.overlay == OverlayCompletion &&
		m.isCompletionActive() &&
		completionPopupHeight(m.height, m.inputLines) <= 6
}

// The local-runtime bootstrap surface — "No local model installed / press p
// to pull qwen3.5:2b" — and the compact "ollama: … try: ollama serve"
// startup projection lived here until both were confirmed unreachable: the
// bootstrap predicate returned false for every remote provider and
// RemoteProvider() is constant-true in this product, and the recovery
// message the projection matched is emitted only on the dead local startup
// branch. local-agent keeps its copies, where a daemon exists to pull into.

// renderWelcome paints only recovery-critical empty-state copy.
//
// On roomy frames the session top bar, composer placeholder, and shortcuts bar
// already own product identity, invite, and model/mode — a mid-canvas
// "SONAR / API-first / Ask…" block is pure noise. Keep a compact
// orientation surface only when session chrome is off (minimum terminals),
// plus bootstrap/offline/Cloud/PLAN boundaries that must stay visible.
func (m *Model) renderWelcome(b *strings.Builder) {
	grid := m.contentGrid()
	contentWidth := m.chatPaneWidth()
	micro := contentWidth < 36
	compact := contentWidth < 58
	headerActive := m.sessionHeaderActive()
	// Left-align at OriginX. Budget uses the remaining pane (including right
	// chrome) so orientation copy keeps the historical fit contract; Line()'s
	// ContentWidth reserve is for structural transcript surfaces.
	writeLine := func(style lipgloss.Style, text string) {
		if micro {
			// 30-column contract: complete safety labels may need the full row.
			b.WriteString(style.Render(truncateDisplayWithGlyphProfile(
				text, max(1, contentWidth), m.glyphProfile,
			)))
		} else {
			budget := max(1, contentWidth-contentLeftColumns)
			b.WriteString(grid.Prefix(" ") + style.Render(
				truncateDisplayWithGlyphProfile(text, budget, m.glyphProfile),
			))
		}
		b.WriteByte('\n')
	}

	plan := m.planStatus()

	// welcomeAmbientLine collects only what this frame assigned to the welcome
	// surface, plus operational exceptions that have no other home. The
	// dedup-by-scanning-infoParts dance this replaces existed because the model
	// name could be appended twice through two different branches.
	// welcomeModeLabel is the authority badge this frame owes the reader, or ""
	// when the welcome does not own it or the mode is the unremarkable default.
	welcomeModeLabel := func() string {
		presentedMode := m.presentedMode()
		if !plan.owns(factMode, surfaceWelcome) || presentedMode == ModeNormal {
			return ""
		}
		switch {
		case presentedMode != ModePlan:
			return m.modeConfigs[presentedMode].Label
		case compact || micro:
			return "PLAN · read-only"
		default:
			return "PLAN · mutation tools removed"
		}
	}

	welcomeAmbientLine := func() string {
		var infoParts []string
		modelLabel := m.currentModelReachabilityLabel(compact)
		if m.currentModelIsNonLocal() {
			if plan.owns(factRemoteBoundary, surfaceWelcome) && modelLabel != "" {
				infoParts = append(infoParts, modelLabel)
			}
		} else if plan.owns(factModel, surfaceWelcome) && modelLabel != "" {
			infoParts = append(infoParts, modelLabel)
		}
		// At the 30-column minimum the remote boundary alone fills the row, so
		// joining the mode after it means the ellipsis eats the authority
		// badge — the exact failure planStatus warns about, a fact assigned to
		// a surface that silently drops it. Both are safety facts and neither
		// should lose to the other, so micro frames give the mode its own row
		// below instead of competing for this one.
		if !micro {
			if mode := welcomeModeLabel(); mode != "" {
				infoParts = append(infoParts, mode)
			}
		}
		// Reachability rides along with the model label above rather than being
		// appended here: a bare "offline" has no subject, and adding it
		// unconditionally printed it a second time on frames where the welcome
		// also owned identity.
		return strings.Join(infoParts, " · ")
	}

	// Roomy chrome owns the empty canvas. Only paint operational exceptions.
	if headerActive {
		if line := welcomeAmbientLine(); line != "" {
			writeLine(m.styles.StatusText, line)
		}
		if micro {
			if mode := welcomeModeLabel(); mode != "" {
				writeLine(m.styles.StatusText, mode)
			}
		}
		return
	}

	// Minimum frames (no session header): keep a short orientation surface.
	writeLine(m.styles.OverlayTitle, "SONAR")
	// Name the provider actually in use rather than a hard-coded runtime; the
	// welcome surface is where a user checks what they are about to pay for.
	trust := "API-first · " + sanitizeTerminalSingleLine(m.activeProviderName()) + " · " + m.approvalPostureWelcomeLabel(false)
	if micro {
		trust = "API-first · " + m.approvalPostureWelcomeMicroLabel()
	} else if compact {
		trust = "API-first · " + m.approvalPostureWelcomeLabel(true)
	}
	writeLine(m.styles.StatusText, trust)

	if line := welcomeAmbientLine(); line != "" {
		writeLine(m.styles.StatusText, line)
	}
	if micro {
		if mode := welcomeModeLabel(); mode != "" {
			writeLine(m.styles.StatusText, mode)
		}
	}

	writeLine(m.styles.WelcomeHint, "Ask · /help")
}

// emptyWelcomeTopPad keeps the empty-state welcome just under the session
// header instead of floating it as a vertically centered island. One blank
// row is enough breathing room; tighter viewports get none.
func emptyWelcomeTopPad(viewportHeight, welcomeHeight int) int {
	if viewportHeight <= 0 || welcomeHeight <= 0 {
		return 0
	}
	available := max(0, viewportHeight-welcomeHeight)
	if available <= 0 {
		return 0
	}
	return 1
}
