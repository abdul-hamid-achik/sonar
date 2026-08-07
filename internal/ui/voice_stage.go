package ui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// The listening stage: what the screen is for when you are not reading it.
//
// The first design for this was a denser transcript — assistant turns collapsed
// to their spoken digest, tool cards folded to a count. That is still a LOG,
// and a log is a thing you read. Somebody listening is not reading; they want
// to know what state this is in, and to be able to reach one detail without
// scrolling a history they have already heard.
//
// So this is not a compressed transcript. It is a single centred panel with the
// present tense on it, and it is centred for a reason beyond taste: it is read
// from across the room, at a glance, by somebody who is not sitting in front of
// it.
//
// # It is a router, not a viewer
//
// Every detail surface this names already exists — alt+d opens the full diff,
// alt+o the full output, ctrl+f searches the transcript, ctrl+t folds the
// focused receipt. Nothing here renders any of them. What was missing was never
// a viewer; it was a home that knows what exists and how to reach it.
//
// That is also why it adds no bindings of its own. The composer stays focused
// on this screen, because a follow-up is exactly what you want to send after
// hearing an answer — so single-letter shortcuts would fight typing. Naming the
// chords that already work costs nothing, conflicts with nothing, and teaches
// the keys the rest of the session uses anyway.

// voiceStageActive reports whether the listening stage owns the screen.
//
// It requires voice to be on. The stage is a way to LISTEN, and a centred panel
// with nothing coming out of the speakers is a worse transcript rather than a
// better one — so turning voice off drops it, rather than leaving a silent
// screen with no explanation.
func (m *Model) voiceStageActive() bool {
	if m == nil || !m.ready || !m.voiceStage || !m.voiceActive() {
		return false
	}
	// It yields to anything that needs an answer or was asked for, and that is
	// what makes it a router rather than a lid.
	//
	// Without this the panel was painted BEFORE the overlay layer, so every
	// destination it advertises was swallowed by it: alt+d opened the diff
	// viewer and the screen did not change, because this returned first. Worse,
	// an approval arriving while the panel was up left its prompt invisible
	// while its keys stayed live — a person answering y or n for something they
	// could not see, under a panel that said only "WAITING FOR YOU". That is
	// exactly the rule this feature is not allowed to break: it may hide prose,
	// never an action.
	//
	// Yielding rather than closing is deliberate. The detour ends and the panel
	// is still there, which is the shape the whole design assumes.
	if m.pendingApproval != nil || m.pendingPaste != nil {
		return false
	}
	if m.overlay != OverlayNone || m.viewerModalActive() {
		return false
	}
	return true
}

// toggleVoiceStage opens or closes it.
func (m *Model) toggleVoiceStage() tea.Cmd {
	if !m.voiceActive() {
		return m.setFooterNotice(noticeInfo,
			"Spoken output is off. Run /voice on first — the stage is for listening.", 5*time.Second)
	}
	m.voiceStage = !m.voiceStage
	if !m.voiceStage {
		m.refreshTranscript()
		return nil
	}
	return nil
}

// voiceStageState is the one line that says what is happening right now.
//
// It reads the same facts the activity rail does, so the two surfaces cannot
// disagree about what the harness is doing — the rail is simply not on screen
// while this is.
func (m *Model) voiceStageState() string {
	switch {
	case m.pendingApproval != nil:
		return "WAITING FOR YOU"
	case m.listeningForVoice():
		return "LISTENING TO YOU"
	case m.state == StateStreaming || m.state == StateWaiting:
		return "WORKING"
	case m.speakingAloud():
		return "SPEAKING"
	default:
		return "IDLE"
	}
}

// voiceStageSummary is the last thing that was said out loud.
//
// The digest, when the model wrote one — which is the point of asking for it:
// a line composed to stand alone is exactly what a glanceable panel wants, and
// it is already being generated for the ear. Without one there is nothing
// honest to put here, so it says so rather than truncating an answer into a
// caption nobody wrote.
func (m *Model) voiceStageSummary() string {
	if digest := strings.TrimSpace(m.voiceLastDigest); digest != "" {
		return digest
	}
	return "—"
}

// voiceStageCounts is what happened, in the only detail a listener can act on:
// how much of it there is, so they know whether a detour is worth taking.
func (m *Model) voiceStageCounts() string {
	var parts []string
	if tools := len(m.toolEntries); tools > 0 {
		parts = append(parts, fmt.Sprintf("%d tool%s", tools, pluralSuffix(tools)))
	}
	if files := len(m.fileChanges); files > 0 {
		parts = append(parts, fmt.Sprintf("%d file%s changed", files, pluralSuffix(files)))
	}
	// Live while a turn runs, settled once it ends. Showing the PREVIOUS turn's
	// duration beside a turn in progress is the one number on this panel that
	// could be read as progress, and it was not moving.
	if elapsed := m.turnElapsed(); elapsed > 0 && (m.state == StateStreaming || m.state == StateWaiting) {
		parts = append(parts, formatWorkingElapsed(elapsed))
	} else if m.lastTurnDuration > 0 {
		parts = append(parts, formatWorkingElapsed(m.lastTurnDuration))
	}
	if len(parts) == 0 {
		return "nothing yet"
	}
	return strings.Join(parts, " · ")
}

// renderVoiceStageView paints the panel.
//
// Centred by the same means the terminal-pause view uses — place each row in
// the full width, then push the block down by half the slack — because that is
// the established way to centre a whole screen here and a second technique
// would drift from it.
func (m *Model) renderVoiceStageView() tea.View {
	width := max(1, m.width)
	height := max(1, m.height)
	content := max(1, width-4)

	rows := []string{
		m.styles.OverlayTitle.Render(truncateDisplay(m.voiceStageState(), content)),
		"",
	}
	// The summary is the only row allowed to wrap: it is a sentence somebody
	// wrote, and cutting it would leave a caption that ends mid-thought.
	for _, line := range strings.Split(wrapText(m.voiceStageSummary(), min(content, 64)), "\n") {
		rows = append(rows, m.styles.StatusText.Render(truncateDisplay(line, content)))
	}
	rows = append(rows,
		"",
		m.styles.OverlayDim.Render(truncateDisplay(m.voiceStageCounts(), content)),
		"",
	)
	// The draft, whenever there is one. This was missing and it mattered more
	// than anything else on the panel: dictation is INSERTED and never sent,
	// precisely so it can be read before it becomes a request — and a screen
	// that hides the composer makes that impossible. Somebody would speak, see
	// nothing, and press enter on words they never checked.
	if draft := strings.TrimSpace(m.input.Value()); draft != "" {
		for _, line := range strings.Split(wrapText("› "+draft, min(content, 64)), "\n") {
			rows = append(rows, m.styles.StatusText.Render(truncateDisplay(line, content)))
		}
		rows = append(rows, "")
	}
	// The doors. Named at whatever width there is, shortest form last, because
	// a panel that overflows is worse than one that offers fewer detours.
	for _, candidate := range []string{
		"alt+d diffs · alt+o output · ctrl+f search · ctrl+t receipts",
		"alt+d diffs · alt+o output · ctrl+f search",
		"alt+d diffs · ctrl+f search",
		"alt+d · ctrl+f",
	} {
		if lipgloss.Width(candidate) <= content {
			rows = append(rows, m.styles.FocusIndicator.Render(candidate))
			break
		}
	}
	leave := []string{
		"esc back to the transcript · " + m.voiceInputKeyHint() + " talk · /voice off",
		"esc transcript · " + m.voiceInputKeyHint() + " talk",
		"esc transcript",
	}
	if strings.TrimSpace(m.input.Value()) != "" {
		leave = append([]string{
			"enter send · esc transcript · " + m.voiceInputKeyHint() + " talk",
			"enter send · esc transcript",
		}, leave...)
	}
	for _, candidate := range leave {
		if lipgloss.Width(candidate) <= content {
			rows = append(rows, m.styles.OverlayDim.Render(candidate))
			break
		}
	}

	if len(rows) > height {
		// Trim from the middle, never the end. The last rows are the way out —
		// esc, the dictation key, /voice off — and a short terminal that clipped
		// the tail kept the prose and dropped every exit, which is the one thing
		// this panel must never do.
		keep := len(rows) - height
		if keep >= len(rows)-2 {
			rows = rows[len(rows)-height:]
		} else {
			rows = append(rows[:1], rows[1+keep:]...)
		}
	}
	for index, row := range rows {
		rows[index] = lipgloss.PlaceHorizontal(width, lipgloss.Center, row)
	}
	body := strings.Join(rows, "\n")
	if top := (height - len(rows)) / 2; top > 0 {
		body = strings.Repeat("\n", top) + body
	}

	view := tea.NewView(body)
	view.AltScreen = true
	view.WindowTitle = m.windowTitleBase() + " · listening"
	m.applyViewTheme(&view)
	return view
}
