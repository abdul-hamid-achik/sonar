package ui

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/viewport"
	"charm.land/lipgloss/v2"
)

type helpRow struct {
	key  string
	desc string
}

// helpContentWidth returns the inner width for the help modal content.
func (m *Model) helpContentWidth() int {
	return pickerListWidth(m.width)
}

// helpViewportHeight returns the viewport height for the help modal.
func (m *Model) helpViewportHeight() int {
	// Shared frame: border (2), title + gap (2), and footer (1).
	h := m.height - 6
	if m.helpContentWidth() < 30 {
		// The minimum-width footer uses a second row so close, page navigation,
		// and endpoint navigation all remain discoverable.
		h--
	}
	if h < 1 {
		h = 1
	}
	return h
}

// buildHelpContent builds the raw help text (without border/viewport wrapper).
//
// Order is deliberate: keyboard / input / voice / waiting first (what you need
// while looking at the screen), then the full slash registry. Slash stays in
// the same overlay so chord documentation tests still see every command, but
// it is no longer competing with the core chords at the top of the scroll.
func (m *Model) buildHelpContent(innerW int) string {
	var b strings.Builder

	b.WriteString(m.styles.OverlayAccent.Render("Keyboard Shortcuts"))
	b.WriteString("\n")

	for index, section := range m.keys.HelpSections() {
		rows := helpRowsForBindings(section.Bindings)
		if len(rows) == 0 {
			continue
		}
		if index > 0 {
			b.WriteString("\n")
		}
		b.WriteString(m.styles.OverlayDim.Render("  " + section.Title))
		b.WriteString("\n")
		m.writeHelpRows(&b, rows, innerW)
	}

	// Select/copy sits above Input Shortcuts on purpose: people look here when
	// a copy does not work, and burying it under paste/@mentions is how that
	// happened. Capture is off by default; the toggle is how to get the wheel.
	b.WriteString("\n")
	b.WriteString(m.styles.OverlayAccent.Render("Select & Copy"))
	b.WriteString("\n")
	m.writeHelpRows(&b, []helpRow{
		{"ctrl+y", "Copy the last assistant reply when the draft is empty"},
		{"alt+m / /mouse", "Turn mouse capture on for wheel-scroll and click-to-expand; drag-select is the default"},
		{"shift+drag", "Still selects while capture is on, in Ghostty, kitty, WezTerm, Alacritty, xterm — Option-drag in iTerm2"},
	}, innerW)

	b.WriteString("\n")
	b.WriteString(m.styles.OverlayAccent.Render("Input Shortcuts"))
	b.WriteString("\n")

	inputShortcuts := []helpRow{
		{"@file / @agent", "Insert file or agent mention text"},
		{"paste/drag images", "Attach up to four PNG, JPEG, or GIF files to the pending prompt"},
		{"~/… or /…", "Review temporary read-only access; MCP tools require separate approval"},
		{"#skill", "Insert skill mention text"},
		{"/cmd", "Run slash command"},
		{"enter (running)", "Slash commands run immediately; other drafts queue until the current turn settles"},
		{"esc (running)", "Clear a queued follow-up first; press again to cancel the turn"},
	}

	m.writeHelpRows(&b, inputShortcuts, innerW)

	// Voice gets its own block because its surface is mostly slash verbs: the
	// keyboard section can only name ctrl+g, and the registry list below only
	// says "/voice" once — neither tells a reader the stage or the channels
	// exist.
	b.WriteString("\n")
	b.WriteString(m.styles.OverlayAccent.Render("Voice"))
	b.WriteString("\n")
	m.writeHelpRows(&b, []helpRow{
		{"ctrl+g", "Open the mic; the same key closes it and the transcription lands in the composer"},
		{"/voice on", "Speak answers and alerts — four channels, each one switchable"},
		{"/voice view", "Listening stage: one panel with the state and the last line said aloud"},
		{"/voice status", "Every voice setting, plus the config block that reproduces the session"},
	}, innerW)

	// The wait trace encodes a fact — is this wait normal for this model? —
	// and a reader who has not been told what the glyphs mean sees decoration.
	b.WriteString("\n")
	b.WriteString(m.styles.OverlayAccent.Render("Waiting Indicator"))
	b.WriteString("\n")

	m.writeHelpRows(&b, []helpRow{
		{"● position", "How far this wait has gone against this model's typical first response"},
		{"│ marker", "Where the reply is expected — left of it is faster than usual"},
		{"● at the edge", "The reply is late; the glyph warns past roughly twice typical"},
		{"first wait", "No typical value yet — the baseline is learned from the second wait on"},
		{"/runtime", "The same numbers without motion: last and typical response time"},
	}, innerW)

	b.WriteString("\n")
	b.WriteString(m.styles.OverlayAccent.Render("Slash Commands"))
	b.WriteString("\n")
	b.WriteString(m.styles.OverlayDim.Render("  Full registry — scroll for actions and availability"))
	b.WriteString("\n")

	if m.cmdRegistry != nil {
		commands := make([]helpRow, 0, len(m.cmdRegistry.All()))
		ctx := m.buildCommandContext()
		for _, cmd := range m.cmdRegistry.All() {
			commands = append(commands, helpRow{key: "/" + cmd.Name, desc: cmd.Description})
			for _, action := range m.cmdRegistry.Actions(cmd.Name, ctx) {
				description := action.Spec.Description
				if !action.Enabled {
					description = "Unavailable · " + action.DisabledReason
				}
				commands = append(commands, helpRow{key: action.Spec.CommandText(), desc: description})
			}
		}
		m.writeHelpRows(&b, commands, innerW)
	}

	b.WriteString("\n")

	return b.String()
}

// helpRowsForBindings projects one group of bindings into presentable rows,
// dropping any binding that carries no help text.
func helpRowsForBindings(bindings []key.Binding) []helpRow {
	rows := make([]helpRow, 0, len(bindings))
	for _, binding := range bindings {
		help := binding.Help()
		if strings.TrimSpace(help.Key) == "" || strings.TrimSpace(help.Desc) == "" {
			continue
		}
		description := strings.TrimSpace(help.Desc)
		description = strings.ToUpper(description[:1]) + description[1:]
		rows = append(rows, helpRow{key: strings.ToLower(help.Key), desc: description})
	}
	return rows
}

// keyHelpRows is every documented binding in section order.
func (m *Model) keyHelpRows() []helpRow {
	var rows []helpRow
	for _, section := range m.keys.HelpSections() {
		rows = append(rows, helpRowsForBindings(section.Bindings)...)
	}
	return rows
}

// writeHelpRows renders aligned rows on normal terminals and stacked rows on
// narrow ones. Descriptions wrap instead of being silently clipped.
func (m *Model) writeHelpRows(b *strings.Builder, rows []helpRow, innerW int) {
	if innerW < 34 {
		for _, row := range rows {
			b.WriteString("  ")
			b.WriteString(m.styles.FocusIndicator.Render(truncateDisplay(row.key, max(1, innerW-3))))
			b.WriteString("\n")
			for _, line := range strings.Split(wrapText(row.desc, max(1, innerW-5)), "\n") {
				b.WriteString("    ")
				b.WriteString(m.styles.OverlayDim.Render(line))
				b.WriteString("\n")
			}
		}
		return
	}

	keyW := 16
	if innerW < 44 {
		keyW = 10
		// Keep common multi-terminal alternatives intact at ordinary narrow
		// widths while reserving enough room for a wrapped description.
		keyCap := max(10, min(22, innerW-16))
		for _, row := range rows {
			keyW = min(keyCap, max(keyW, lipgloss.Width(row.key)))
		}
	}
	// Leave the terminal's final cell unused. Writing exactly to the edge can
	// trigger an implicit wrap before the explicit newline in some PTYs.
	descW := max(1, innerW-keyW-5)
	for _, row := range rows {
		descLines := strings.Split(wrapText(row.desc, descW), "\n")
		for i, line := range descLines {
			if i == 0 {
				fmt.Fprintf(b, "  %s  %s\n",
					m.styles.FocusIndicator.Width(keyW).Render(truncateDisplay(row.key, keyW)),
					m.styles.OverlayDim.Render(line),
				)
				continue
			}
			b.WriteString(strings.Repeat(" ", keyW+4))
			b.WriteString(m.styles.OverlayDim.Render(line))
			b.WriteString("\n")
		}
	}
}

// initHelpViewport creates and populates the help viewport for scrolling.
func (m *Model) initHelpViewport() {
	m.resizeHelpViewport(false)
}

func (m *Model) resizeHelpViewport(preserveOffset bool) {
	offset := 0
	if preserveOffset {
		offset = m.helpViewport.YOffset()
	}
	innerW := m.helpContentWidth()
	vpH := m.helpViewportHeight()

	m.helpViewport = viewport.New(
		viewport.WithWidth(innerW),
		viewport.WithHeight(vpH),
	)
	// Disable default arrow key bindings (we handle j/k/up/down ourselves via parent)
	m.helpViewport.KeyMap.Up.SetEnabled(false)
	m.helpViewport.KeyMap.Down.SetEnabled(false)
	m.helpViewport.KeyMap.PageUp.SetEnabled(false)
	m.helpViewport.KeyMap.PageDown.SetEnabled(false)
	m.helpViewport.KeyMap.HalfPageUp.SetEnabled(false)
	m.helpViewport.KeyMap.HalfPageDown.SetEnabled(false)

	content := m.buildHelpContent(innerW)
	m.helpViewport.SetContent(content)
	m.helpViewport.SetYOffset(offset)
}

// renderHelpOverlay builds a centered, scrollable help modal.
func (m *Model) renderHelpOverlay(_ int) string {
	innerW := m.helpContentWidth()

	var b strings.Builder

	// Title.
	b.WriteString(m.styles.OverlayTitle.Render("Help"))
	b.WriteString("\n\n")

	// Viewport content (scrollable).
	b.WriteString(m.helpViewport.View())
	b.WriteString("\n")

	// Scroll indicator / footer.
	pct := m.helpViewport.ScrollPercent()
	hints := []keyHint{{Key: "esc/q", Action: m.overlayCloseLabel()}}
	if pct <= 0 {
		hints = append(hints,
			keyHint{Key: "pgdn", Action: "more"},
			keyHint{Key: "g/shift+g", Action: "ends"},
		)
	} else if pct >= 1.0 {
		hints = append(hints,
			keyHint{Key: "j/k", Action: "scroll"},
			keyHint{Key: "g", Action: "top"},
		)
	} else {
		hints = append(hints,
			keyHint{Key: "j/k", Action: "scroll"},
			keyHint{Key: "pgup/pgdn", Action: "page"},
			keyHint{Key: fmt.Sprintf("%.0f%%", pct*100)},
		)
	}

	footer := m.renderKeyHints(innerW, hints...)
	if innerW < 30 {
		var navigation []keyHint
		switch {
		case pct <= 0:
			navigation = []keyHint{{Key: "pgdn", Action: "more"}, {Key: "g/⇧g", Action: "ends"}}
		case pct >= 1.0:
			navigation = []keyHint{{Key: "j/k", Action: "scroll"}, {Key: "g", Action: "top"}}
		default:
			navigation = []keyHint{{Key: "pgup/dn", Action: "page"}, {Key: "g/⇧g", Action: "ends"}}
		}
		footer = m.renderKeyHints(innerW, keyHint{Key: "esc/q", Action: m.overlayCloseLabel()}) + "\n" +
			m.renderKeyHints(innerW, navigation...)
	}

	return m.renderPickerFrame(b.String(), footer)
}

// overlayOnContent paints a modal over the base frame on an anchored scrim.
//
// The scrim is not decoration. Letting the base show through on the modal's own
// rows meant the transcript/composer rule ran straight through the panel and
// the shortcuts row was sliced mid-word at the border, which reads as a paint
// bug rather than as depth. Every row the modal occupies — plus one clear row
// above and below — now belongs to the modal.
func (m *Model) overlayOnContent(base, overlay string) string {
	baseLines := strings.Split(base, "\n")
	overlayLines := strings.Split(overlay, "\n")
	startY := centeredOverlayStartY(base, overlay)
	startX := centeredOverlayLineX(m.width, overlay)
	canvasWidth := max(m.width, lipgloss.Width(base), lipgloss.Width(overlay))
	canvas := lipgloss.NewCanvas(canvasWidth, len(baseLines))
	scrimWidth := max(1, m.width)

	// scrim(+2) covers one clear row on each side of the panel so the modal
	// never appears welded to surrounding chrome.
	layers := make([]*lipgloss.Layer, 0, 2*len(overlayLines)+3)
	layers = append(layers, lipgloss.NewLayer(base).Z(0))
	for row := startY - 1; row <= startY+len(overlayLines); row++ {
		if row < 0 || row >= len(baseLines) {
			continue
		}
		layers = append(layers, lipgloss.NewLayer(strings.Repeat(" ", scrimWidth)).
			Y(row).
			Z(1))
	}

	for i, ol := range overlayLines {
		row := startY + i
		if row >= len(baseLines) {
			break
		}
		// One X for the whole block: ragged rows inside a border must not drift.
		layers = append(layers, lipgloss.NewLayer(ol).
			X(startX).
			Y(row).
			Z(2))
	}

	// Lip Gloss' cell compositor keeps ANSI styles and grapheme widths intact.
	canvas.Compose(lipgloss.NewCompositor(layers...))
	return canvas.Render()
}
