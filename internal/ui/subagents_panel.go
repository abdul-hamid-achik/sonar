package ui

import (
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"

	"github.com/abdul-hamid-achik/sonar/internal/agent"
)

// The subagents view is the switcher the feature exists for: every child on
// one screen, the selected child's activity and answer-so-far underneath —
// present tense while it runs, the settled answer once it stops. It reads
// live Agent snapshots on every rebuild, so "live" is nothing more than a
// repaint tick while the view is open. It owns the whole screen, like the
// voice stage, rather than sitting in a modal over a conversation nobody is
// reading meanwhile.
//
// Selection and scroll deliberately use different axes (←/→ switches a
// child, ↑/↓ and page keys scroll its transcript) so switching children —
// the whole point — never fights reading one of them.
type SubagentsPanelState struct {
	Viewport viewport.Model
	Selected int
}

// subagentsPanelTickMsg forces a repaint while the panel is open; children
// run in their own processes and deliver no Bubble Tea messages of their own.
type subagentsPanelTickMsg struct{}

const subagentsPanelTickInterval = 500 * time.Millisecond

func (m *Model) openSubagentsPanel() tea.Cmd {
	if m.subagentsPanelState == nil {
		m.subagentsPanelState = &SubagentsPanelState{}
	}
	m.refreshSubagentsPanel(false)
	m.overlay = OverlaySubagents
	m.input.Blur()
	return m.subagentsPanelTick()
}

func (m *Model) closeSubagentsPanel() {
	m.subagentsPanelState = nil
	m.closeOverlayToParent()
}

func (m *Model) subagentsPanelTick() tea.Cmd {
	return tea.Tick(subagentsPanelTickInterval, func(time.Time) tea.Msg {
		return subagentsPanelTickMsg{}
	})
}

func (m *Model) handleSubagentsPanelTick() tea.Cmd {
	if m.overlay != OverlaySubagents || m.subagentsPanelState == nil {
		return nil
	}
	m.refreshSubagentsPanel(true)
	return m.subagentsPanelTick()
}

func (m *Model) refreshSubagentsPanel(preserveOffset bool) {
	state := m.subagentsPanelState
	if state == nil {
		return
	}
	offset := 0
	if preserveOffset {
		offset = state.Viewport.YOffset()
	}
	// The view owns the whole screen: tabs row + title above, hints below.
	width := max(20, m.width-4)
	content := m.buildSubagentsContent(width)
	height := max(3, m.height-6)
	vp := viewport.New(
		viewport.WithWidth(width),
		viewport.WithHeight(height),
	)
	vp.KeyMap.Up.SetEnabled(false)
	vp.KeyMap.Down.SetEnabled(false)
	vp.KeyMap.PageUp.SetEnabled(false)
	vp.KeyMap.PageDown.SetEnabled(false)
	vp.KeyMap.HalfPageUp.SetEnabled(false)
	vp.KeyMap.HalfPageDown.SetEnabled(false)
	vp.SetContent(content)
	vp.SetYOffset(offset)
	state.Viewport = vp
}

func (m *Model) subagentSnapshotsForPanel() []agent.SubagentSnapshot {
	if m.agent == nil {
		return nil
	}
	return m.agent.SubagentSnapshots()
}

func (m *Model) buildSubagentsContent(width int) string {
	snapshots := m.subagentSnapshotsForPanel()
	state := m.subagentsPanelState
	if len(snapshots) == 0 {
		return "No subagents in this session.\n\nThe model starts one with the agent tool; finished children keep\ntheir full transcript in the session picker."
	}
	if state.Selected >= len(snapshots) {
		state.Selected = len(snapshots) - 1
	}
	if state.Selected < 0 {
		state.Selected = 0
	}

	var b strings.Builder
	for index, snapshot := range snapshots {
		marker := "  "
		if index == state.Selected {
			marker = "▸ "
		}
		label := snapshot.ID
		if snapshot.Provider != "" && snapshot.Provider != "sonar" {
			label += " [" + snapshot.Provider + "]"
		}
		if snapshot.Name != "" {
			label += " (" + snapshot.Name + ")"
		}
		fmt.Fprintf(&b, "%s%s · %s", marker, label, snapshot.Status)
		if snapshot.EvalTokens > 0 {
			fmt.Fprintf(&b, " · %d tok", snapshot.EvalTokens)
		}
		if snapshot.ToolCalls > 0 {
			fmt.Fprintf(&b, " · %d tools", snapshot.ToolCalls)
		}
		if snapshot.Status == "running" {
			if now := latestSubagentActivity(snapshot); now != "" {
				fmt.Fprintf(&b, " · %s", now)
			}
		}
		b.WriteString("\n")
	}

	selected := snapshots[state.Selected]
	b.WriteString(strings.Repeat("─", max(8, min(width-2, 120))))
	b.WriteString("\n")
	prompt := sanitizeTerminalSingleLine(selected.Prompt)
	if runes := []rune(prompt); len(runes) > 160 {
		prompt = string(runes[:159]) + "…"
	}
	fmt.Fprintf(&b, "task: %s\n", prompt)
	if selected.SessionRef != "" {
		fmt.Fprintf(&b, "transcript: session %s (session picker keeps it after this panel closes)\n", selected.SessionRef)
	}
	b.WriteString("\n")

	events := selected.Events
	if len(events) > 12 {
		events = events[len(events)-12:]
	}
	for _, event := range events {
		switch event.Kind {
		case "tool_start":
			fmt.Fprintf(&b, "→ %s\n", event.Name)
		case "tool_result":
			fmt.Fprintf(&b, "✓ %s · %s · %dms\n", event.Name, event.Status, event.DurationMS)
		case "error":
			fmt.Fprintf(&b, "! %s\n", sanitizeTerminalSingleLine(event.Message))
		}
	}
	if text := strings.TrimSpace(selected.Text); text != "" {
		b.WriteString("\n")
		b.WriteString(sanitizeTerminalMultiline(text))
		b.WriteString("\n")
	}
	if selected.Status == "failed" && selected.StopReason != "" {
		fmt.Fprintf(&b, "\nstop reason: %s\n", sanitizeTerminalSingleLine(selected.StopReason))
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderSubagentsView owns the entire screen, like the voice stage: watching
// parallel workers is a full-attention activity, not a modal over a
// conversation nobody is reading meanwhile. It may hide prose, never an
// action and never an error.
func (m *Model) renderSubagentsView() tea.View {
	vp := &m.subagentsPanelState.Viewport
	var b strings.Builder
	b.WriteString(m.styles.OverlayTitle.Render("Subagents"))
	b.WriteString("\n\n")
	b.WriteString(vp.View())
	b.WriteString("\n")
	hints := []keyHint{
		{Key: "esc", Action: "back"},
		{Key: "←/→", Action: "switch subagent"},
		{Key: "↑/↓", Action: "scroll"},
	}
	if !vp.AtBottom() {
		hints = append(hints, keyHint{Key: "↓", Action: "more"})
	}
	b.WriteString(m.renderKeyHints(max(20, m.width-4), hints...))

	view := tea.NewView(b.String())
	view.AltScreen = true
	view.WindowTitle = m.windowTitleBase() + " · agents"
	m.applyViewTheme(&view)
	return view
}

func latestSubagentActivity(snapshot agent.SubagentSnapshot) string {
	for i := len(snapshot.Events) - 1; i >= 0; i-- {
		event := snapshot.Events[i]
		if event.Kind == "tool_start" || event.Kind == "tool_result" {
			return "now: " + event.Name
		}
	}
	return ""
}

// moveSubagentSelection switches the detailed child and resets the scroll so
// the new child's activity starts at the top.
func (m *Model) moveSubagentSelection(delta int) {
	state := m.subagentsPanelState
	if state == nil {
		return
	}
	count := len(m.subagentSnapshotsForPanel())
	if count == 0 {
		return
	}
	state.Selected = (state.Selected + delta + count) % count
	m.refreshSubagentsPanel(false)
}
