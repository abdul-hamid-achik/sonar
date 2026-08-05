package ui

import (
	"strings"
	"testing"
)

// At the 30-column minimum the welcome ambient line was
// "DEEPSEEK · remote prompts · PLAN · read-only", joined and then truncated to
// the row — so the ellipsis ate the authority badge and the reader was told
// their prompts leave the machine but not that the session could act on its
// own.
//
// That is precisely the failure planStatus documents: a fact assigned to a
// surface that silently drops it is a fact lost entirely. Both are safety
// facts, so neither loses to the other; the mode gets its own row instead of
// competing for one.
func TestMinimumWidthKeepsBothSafetyFacts(t *testing.T) {
	for _, test := range []struct {
		name string
		mode Mode
		want string
	}{
		{name: "plan", mode: ModePlan, want: "PLAN"},
		{name: "auto", mode: ModeAuto, want: "AUTO"},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := newTestModel(t)
			m.width = 30
			m.height = 12
			m.recalcViewportHeight()
			m.mode = test.mode
			withRemoteProvider(t, m)
			m.model = "deepseek-v4-flash"

			var b strings.Builder
			m.renderWelcome(&b)
			frame := b.String()

			if !strings.Contains(frame, "remote prompts") {
				t.Errorf("the remote boundary was lost at 30 columns:\n%s", frame)
			}
			want := test.want
			if !strings.Contains(frame, want) {
				t.Errorf("the %s authority badge was lost at 30 columns:\n%s", want, frame)
			}
			// Each fact on its own row is the point; a joined row is what
			// truncated.
			for _, line := range strings.Split(frame, "\n") {
				if strings.Contains(line, "remote prompts") && strings.Contains(line, want) {
					t.Errorf("both facts share one row again and will truncate: %q", line)
				}
			}
		})
	}
}

// NORMAL is the unremarkable default. Printing it would spend a row of a
// twelve-row terminal saying nothing has changed.
func TestMinimumWidthStaysQuietInNormal(t *testing.T) {
	m := newTestModel(t)
	m.width = 30
	m.height = 12
	m.recalcViewportHeight()
	m.mode = ModeNormal
	withRemoteProvider(t, m)

	var b strings.Builder
	m.renderWelcome(&b)
	if frame := b.String(); strings.Contains(frame, "NORMAL") {
		t.Errorf("the default authority spent a row at minimum width:\n%s", frame)
	}
}

// The joined form is still what a frame with room uses — the split is a
// minimum-width accommodation, not a new layout. Asserting it through the
// welcome is not possible at a roomy width, where the top bar owns these facts
// and the welcome paints nothing at all, so this pins the branch directly.
func TestOnlyMicroFramesSplitTheFacts(t *testing.T) {
	m := newTestModel(t)
	m.mode = ModePlan
	withRemoteProvider(t, m)

	// A compact-but-not-micro pane joins; only below 36 columns does the mode
	// move to its own row.
	m.width = 50
	m.height = 20
	m.recalcViewportHeight()
	if m.chatPaneWidth() < 36 {
		t.Skipf("pane is %d columns; fixture is not in the compact band", m.chatPaneWidth())
	}
	var compactFrame strings.Builder
	m.renderWelcome(&compactFrame)

	m.width = 30
	m.height = 12
	m.recalcViewportHeight()
	var microFrame strings.Builder
	m.renderWelcome(&microFrame)

	countRowsWithBoth := func(frame string) int {
		rows := 0
		for _, line := range strings.Split(frame, "\n") {
			if strings.Contains(line, "remote prompts") && strings.Contains(line, "PLAN") {
				rows++
			}
		}
		return rows
	}
	if countRowsWithBoth(microFrame.String()) != 0 {
		t.Errorf("a micro frame still joined the two facts:\n%s", microFrame.String())
	}
	if !strings.Contains(microFrame.String(), "PLAN") {
		t.Errorf("a micro frame lost the authority badge:\n%s", microFrame.String())
	}
}
