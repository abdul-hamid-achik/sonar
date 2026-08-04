package ui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/rivo/uniseg"
)

func (m *Model) View() tea.View {
	if !m.ready {
		// Bubble Tea has not delivered terminal dimensions yet, so centering would
		// be guesswork. Keep the same product identity and startup language as the
		// full shell instead of flashing an unrelated debug placeholder.
		v := tea.NewView("LOCAL AGENT\nStarting…")
		v.AltScreen = true
		v.WindowTitle = m.windowTitleBase()
		m.applyViewTheme(&v)
		return v
	}
	if hint := m.narrowTerminalHint(); hint != "" {
		return m.renderNarrowTerminalView(hint)
	}
	if m.terminalInputResumeActive() {
		return m.renderTerminalInputResumeView()
	}

	// Session header + conversation + footer share one geometry snapshot.
	// Infrequent controls remain overlays over these stable base rectangles.
	m.syncTranscriptPaintWindow()
	frame := m.projectFrame()
	// Keep the Bubbles viewport geometry locked to the projected transcript
	// rect so footer chrome changes (framed composer, sticky header) cannot
	// leave a stale tall viewport and overflow the terminal height.
	if h := frame.Transcript.Rect.Height(); h > 0 && m.viewport.Height() != h {
		m.viewport.SetHeight(h)
	}
	if w := frame.Transcript.Rect.Width(); w > 0 && m.viewport.Width() != w {
		m.viewport.SetWidth(w)
	}
	var content strings.Builder
	if frame.Header.Visible && frame.Header.Content != "" {
		content.WriteString(frame.Header.Content)
		content.WriteString("\n")
	}
	content.WriteString(m.viewport.View())
	content.WriteString("\n")
	paintedFooterY := strings.Count(content.String(), "\n")
	content.WriteString(frame.Footer.Content)
	viewCursor := frame.Cursor
	if viewCursor != nil && paintedFooterY != frame.Footer.Rect.MinY {
		// Tests and a few setup paths can replace an owner immediately before a
		// viewport reflow. Keep the single projected local cursor, but translate
		// it to the footer's actually painted origin for that transitional frame.
		viewCursor = offsetCursor(viewCursor, 0, paintedFooterY-frame.Footer.Rect.MinY)
	}

	// Infrequent controls remain centered overlays. Composer-owned completion,
	// transcript search, Plan, and Goal surfaces were already rendered in the
	// normal footer flow.
	if m.overlay != OverlayNone && m.overlay != OverlayCompletion &&
		m.overlay != OverlayTranscriptSearch &&
		m.overlay != OverlayCortexDecision &&
		m.overlay != OverlayPlanForm && m.overlay != OverlayGoalForm {
		var overlay string
		var localCursor *tea.Cursor
		// Every overlay suppresses the underlying composer cursor. Text-entry
		// overlays may replace it with their own translated child cursor below.
		viewCursor = nil
		switch m.overlay {
		case OverlayHelp:
			overlay = m.renderHelpOverlay(m.width)
		case OverlayModelPicker:
			if m.modelPickerState != nil {
				overlay = m.renderModelPicker()
			}
		case OverlaySessionsPicker:
			if m.sessionsPickerState != nil {
				overlay = m.renderSessionsPicker()
			}
		case OverlaySettings:
			overlay = m.renderSettingsPicker()
		case OverlayPermissions:
			overlay = m.renderPermissionsPanel()
		case OverlayAgentPicker:
			overlay = m.renderAgentPicker()
		case OverlayProviderPicker:
			overlay = m.renderProviderPicker()
		case OverlayAgents:
			overlay = m.renderAgentHub()
		case OverlayModePicker:
			overlay = m.renderModePicker()
		case OverlayThemePicker:
			overlay = m.renderThemePicker()
		case OverlayContextDoctor:
			overlay = m.renderContextDoctor()
		case OverlayRuntimeStatus:
			overlay = m.renderRuntimeStatus()
		case OverlayGoalInspector:
			if m.goalInspectorState != nil {
				overlay = m.goalInspectorState.View()
			}
		case OverlayGoalRecovery:
			if m.goalRecoveryState != nil {
				overlay, localCursor = m.goalRecoveryState.ViewWithCursor()
			}
		}
		if overlay != "" {
			base := content.String()
			content.Reset()
			content.WriteString(m.overlayOnContent(base, overlay))
			viewCursor = overlayCursor(base, overlay, m.width, localCursor)
		}
	}
	if m.viewerModalActive() {
		viewCursor = nil
		if composed, modalCursor, ok := m.composeViewerModal(content.String()); ok {
			content.Reset()
			content.WriteString(composed)
			viewCursor = modalCursor
		}
	}

	v := tea.NewView(content.String() + "\n")
	v.AltScreen = true
	// Cell-motion reports wheel + clicks for tool hit targets. Native terminal
	// selection still works via Shift-drag in most terminals (iTerm, Ghostty,
	// Kitty, WezTerm). Ctrl+Y copies the last assistant message when idle.
	// Prefer Shift-drag over disabling mouse: wheel scroll of the transcript
	// is a daily-driver affordance.
	v.MouseMode = tea.MouseModeCellMotion
	v.Cursor = viewCursor
	m.applyViewTheme(&v)

	// Terminal title progress. The workspace basename differentiates several
	// sonar tabs without exposing a full private path through terminal
	// title integrations or window-manager history.
	windowTitle := m.windowTitleBase()
	switch m.state {
	case StateWaiting:
		v.WindowTitle = windowTitle + " \u00b7 thinking..."
	case StateStreaming:
		v.WindowTitle = windowTitle + " \u00b7 streaming..."
	default:
		if m.hasSuccessFooterNotice() {
			v.WindowTitle = windowTitle + " \u00b7 done"
		} else {
			v.WindowTitle = windowTitle
		}
	}

	return v
}

// applyViewTheme sets Bubble Tea's terminal-level surface color. This paints
// cells not owned by a component (including resize gaps) without padding the
// rendered content or changing any ContentGrid geometry.
func (m *Model) applyViewTheme(view *tea.View) {
	if m == nil || view == nil || noColor {
		return
	}
	view.BackgroundColor = newSemanticPalette(m.isDark, m.themeID).Background
}

// activityComposerGap used to insert a blank row between the live activity rail
// and the composer. Grok-style density keeps them adjacent; the framed composer
// already provides its own top edge, so the gap is never requested.
func (m *Model) activityComposerGap() bool {
	return false
}

func (m *Model) conversationStarted() bool {
	for _, entry := range m.entries {
		switch entry.Kind {
		case "user", "assistant", "tool_group":
			return true
		}
	}
	return false
}

func (m *Model) inspectableToolReceiptAction() (string, bool) {
	if m.lastTurnToolIndex < 0 || m.lastTurnToolIndex >= len(m.toolEntries) ||
		m.toolEntries[m.lastTurnToolIndex].Status == ToolStatusRunning {
		return "", false
	}
	if m.toolEntries[m.lastTurnToolIndex].Collapsed {
		return "inspect receipt", true
	}
	return "hide receipt", true
}

// formatTokens formats a token count as "1.0M", "1.2k", or "999". The M tier
// keeps Cloud-scale context windows readable in the absolute meter
// ("120.4k/1.0M" instead of "120.4k/1048.6k").
func formatTokens(n int) string {
	if n >= 1_000_000 {
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	}
	if n >= 1000 {
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprintf("%d", n)
}

// formatDuration formats a duration as "42ms" or "1.3s".
func formatDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}

// truncateDisplay truncates plain text by terminal cell width instead of byte
// count, so model names, paths, and tool output containing Unicode stay valid.
func truncateDisplay(s string, maxWidth int) string {
	return truncateDisplayWithGlyphProfile(s, maxWidth, GlyphUnicode)
}

func truncateDisplayWithGlyphProfile(s string, maxWidth int, profile GlyphProfile) string {
	if maxWidth <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= maxWidth {
		return s
	}
	marker := "…"
	if resolveGlyphProfile(profile) == GlyphASCII {
		marker = "~"
	}
	if maxWidth <= lipgloss.Width(marker) {
		return marker
	}

	budget := maxWidth - lipgloss.Width(marker)
	var b strings.Builder
	used := 0
	graphemes := uniseg.NewGraphemes(s)
	for graphemes.Next() {
		cluster := graphemes.Str()
		clusterWidth := lipgloss.Width(cluster)
		if used+clusterWidth > budget {
			break
		}
		b.WriteString(cluster)
		used += clusterWidth
	}
	return b.String() + marker
}

// wrapText wraps text to the given width, breaking long words if needed.
func wrapText(s string, width int) string {
	if width <= 0 {
		return s
	}
	// Fast path: if a single line fits in terminal cells, return as-is.
	if !strings.Contains(s, "\n") && lipgloss.Width(s) <= width {
		return s
	}
	var result strings.Builder
	for _, line := range strings.Split(s, "\n") {
		result.WriteString(wrapLine(line, width))
		result.WriteString("\n")
	}
	// Trim trailing newline
	return strings.TrimSuffix(result.String(), "\n")
}

// wrapChunk is one wrapped row plus the byte offset in the source line where
// that row begins. The offset is what lets the incremental live-tail resume
// wrapping mid-line instead of re-wrapping the whole message per token.
type wrapChunk struct {
	text     string
	rawStart int
}

// wrapLineChunks is the single line-wrapping algorithm in the package.
//
// It used to exist twice: once here returning plain strings, and once in
// transcript_paint.go returning the same rows with source offsets attached.
// Two copies of word packing and grapheme splitting had to stay identical for
// the incremental paint path to keep matching the canonical render — a
// correctness coupling that nothing enforced. Callers that do not need offsets
// project them away instead.
//
// forceWrap makes a line that already fits still go through packing, which the
// live tail needs when a previous chunk ended mid-line.
func wrapLineChunks(line string, width int, forceWrap bool) ([]wrapChunk, bool) {
	if width <= 0 {
		return []wrapChunk{{text: line}}, false
	}
	if !forceWrap && lipgloss.Width(line) <= width {
		return []wrapChunk{{text: line}}, false
	}
	words := strings.Fields(line)
	if len(words) == 0 {
		return []wrapChunk{{text: "", rawStart: len(line)}}, true
	}

	chunks := make([]wrapChunk, 0, len(words))
	current := wrapChunk{}
	searchStart := 0
	for _, word := range words {
		relative := strings.Index(line[searchStart:], word)
		if relative < 0 {
			// strings.Fields returns substrings of line, so this is defensive.
			relative = 0
		}
		wordStart := searchStart + relative
		searchStart = wordStart + len(word)
		if current.text != "" &&
			lipgloss.Width(current.text)+1+lipgloss.Width(word) <= width {
			current.text += " " + word
			continue
		}
		if current.text != "" {
			chunks = append(chunks, current)
			current = wrapChunk{}
		}

		split := splitDisplayChunksAt(word, wordStart, width)
		if len(split) == 0 {
			continue
		}
		if len(split) > 1 {
			chunks = append(chunks, split[:len(split)-1]...)
		}
		current = split[len(split)-1]
	}
	if current.text != "" {
		chunks = append(chunks, current)
	}
	if len(chunks) == 0 {
		return []wrapChunk{{text: "", rawStart: len(line)}}, true
	}
	return chunks, true
}

// wrapLine wraps a single line to the given width, breaking long words if needed.
func wrapLine(line string, width int) string {
	chunks, _ := wrapLineChunks(line, width, false)
	rows := make([]string, len(chunks))
	for index := range chunks {
		rows[index] = chunks[index].text
	}
	return strings.Join(rows, "\n")
}

// splitDisplayChunksAt splits one long word without slicing through UTF-8 and
// measures terminal cells, which matters for CJK and emoji model output. Each
// chunk carries its byte offset relative to the enclosing line.
func splitDisplayChunksAt(word string, wordStart, width int) []wrapChunk {
	if word == "" || width <= 0 {
		return nil
	}
	var chunks []wrapChunk
	var chunk strings.Builder
	chunkStart := 0
	used := 0
	graphemes := uniseg.NewGraphemes(word)
	for graphemes.Next() {
		start, _ := graphemes.Positions()
		cluster := graphemes.Str()
		clusterWidth := lipgloss.Width(cluster)
		if used > 0 && used+clusterWidth > width {
			chunks = append(chunks, wrapChunk{text: chunk.String(), rawStart: wordStart + chunkStart})
			chunk.Reset()
			chunkStart = start
			used = 0
		}
		if chunk.Len() == 0 {
			chunkStart = start
		}
		chunk.WriteString(cluster)
		used += clusterWidth
		if used >= width {
			chunks = append(chunks, wrapChunk{text: chunk.String(), rawStart: wordStart + chunkStart})
			chunk.Reset()
			used = 0
		}
	}
	if chunk.Len() > 0 {
		chunks = append(chunks, wrapChunk{text: chunk.String(), rawStart: wordStart + chunkStart})
	}
	return chunks
}

// splitDisplayChunks is the text-only projection of splitDisplayChunksAt.
func splitDisplayChunks(word string, width int) []string {
	chunks := splitDisplayChunksAt(word, 0, width)
	if len(chunks) == 0 {
		return nil
	}
	rows := make([]string, len(chunks))
	for index := range chunks {
		rows[index] = chunks[index].text
	}
	return rows
}
