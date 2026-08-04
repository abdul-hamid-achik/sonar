package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// This is the guard the codebase was missing. Duplicated ambient state was
// found by eye, one pair at a time, and fixed with a local `if !headerActive`
// that only covered the pair someone had noticed. These tests fail on any
// repetition across every supported width, so the next one cannot ship quietly.

// ambientFrames are the frame sizes and session states worth sweeping: the
// supported minimum, the widths where the top bar and the shortcuts identity
// switch on, and a roomy desktop terminal.
type ambientFrame struct {
	name          string
	width, height int
}

var ambientFrames = []ambientFrame{
	{"minimum", 30, 12},
	{"narrow", 46, 18},
	{"identity threshold", 52, 20},
	{"standard", 80, 24},
	{"wide", 120, 36},
}

func TestAmbientStateIsNeverPrintedTwiceInAFrame(t *testing.T) {
	// The first cut of this test only exercised an idle frame at ~1% context
	// occupancy with no goal attached, and passed while four surfaces — the
	// activity rail, the goal row, the context-pressure promotion and the
	// compact repack — printed ambient facts twice. The states below are the
	// ones that were missing.
	states := []struct {
		name  string
		apply func(m *Model)
	}{
		{"idle", func(*Model) {}},
		{"streaming", func(m *Model) {
			m.state = StateStreaming
			m.turnStartedAt = m.nowTime()
			m.reducedMotion = true
		}},
		{"context pressure", func(m *Model) { m.promptTokens = m.numCtx * 9 / 10 }},
	}
	for _, mode := range []Mode{ModeNormal, ModePlan, ModeAuto} {
		for _, started := range []bool{false, true} {
			for _, frame := range ambientFrames {
				m := newTestModel(t)
				updated, _ := m.Update(tea.WindowSizeMsg{Width: frame.width, Height: frame.height})
				m = updated.(*Model)
				m.model = "ornith:latest"
				m.setMode(mode)
				m.numCtx = 98_304
				m.promptTokens = 1024
				if started {
					m.entries = []ChatEntry{
						{Kind: "user", Content: "hey"},
						{Kind: "assistant", Content: "sure"},
					}
				}
				for _, state := range states {
					state.apply(m)
					m.recalcViewportHeight()
					m.refreshTranscript()

					view := ansi.Strip(m.View().Content)
					label := frame.name + "/" + m.modeConfigs[mode].Label + "/" + state.name
					if started {
						label += "/started"
					}

					if got := strings.Count(view, "ornith"); got > 1 {
						t.Errorf("%s: model printed %d times:\n%s", label, got, view)
					}
					// NORMAL is never badged, so only PLAN and AUTO can double up.
					if mode != ModeNormal {
						if got := strings.Count(view, m.modeConfigs[mode].Label); got > 1 {
							t.Errorf("%s: mode printed %d times:\n%s", label, got, view)
						}
					}
					// The meter is one fact: "N/M" must not appear twice.
					if meter := ansi.Strip(m.renderContextStatus()); meter != "" {
						if got := strings.Count(view, meter); got > 1 {
							t.Errorf("%s: context meter printed %d times:\n%s", label, got, view)
						}
					}
				}
			}
		}
	}
}

// Ownership must also be total: a fact assigned to a surface that will not
// paint it is a fact the reader never sees. This is the failure mode that the
// first cut of planStatus shipped, by assigning identity to the shortcuts row
// at widths where that row carries keys only.
func TestAmbientStateAlwaysHasExactlyOneOwner(t *testing.T) {
	for _, frame := range ambientFrames {
		for _, started := range []bool{false, true} {
			m := newTestModel(t)
			updated, _ := m.Update(tea.WindowSizeMsg{Width: frame.width, Height: frame.height})
			m = updated.(*Model)
			m.model = "ornith:latest"
			m.setMode(ModePlan)
			if started {
				m.entries = []ChatEntry{{Kind: "user", Content: "hey"}}
			}
			m.recalcViewportHeight()

			plan := m.planStatus()
			for fact, name := range map[statusFact]string{
				factModel:          "model",
				factRemoteBoundary: "remote boundary",
				factContext:        "context",
				factMode:           "mode",
			} {
				if plan.ownedBy(fact) == surfaceNone {
					t.Errorf("%s (started=%v): %s has no owner", frame.name, started, name)
				}
			}
		}
	}
}

// The model and the authority badge must each be reachable on every supported
// frame. A no-duplication rule is trivially satisfiable by printing nothing.
func TestAmbientStateRemainsVisibleOnEverySupportedFrame(t *testing.T) {
	for _, frame := range ambientFrames {
		m := newTestModel(t)
		updated, _ := m.Update(tea.WindowSizeMsg{Width: frame.width, Height: frame.height})
		m = updated.(*Model)
		m.model = "ornith:latest"
		m.setMode(ModePlan)
		m.entries = []ChatEntry{{Kind: "user", Content: "hey"}}
		m.recalcViewportHeight()
		m.refreshTranscript()

		view := ansi.Strip(m.View().Content)
		if !strings.Contains(view, "ornith") {
			t.Errorf("%s: model is not visible anywhere:\n%s", frame.name, view)
		}
		if !strings.Contains(view, "PLAN") {
			t.Errorf("%s: PLAN authority is not visible anywhere:\n%s", frame.name, view)
		}
	}
}

// A theme switch must reach every cached surface, not just the styles the
// Model holds directly. Child components, Bubbles delegates, and the Glamour
// renderer each keep their own copy, which is what rebuildThemedSurfaces
// exists to repaint.
func TestThemeSwitchRepaintsTheWholeFrame(t *testing.T) {
	previous := noColor
	noColor = false
	t.Cleanup(func() { noColor = previous })

	m := newTestModel(t)
	m.entries = []ChatEntry{
		{Kind: "user", Content: "hey"},
		{Kind: "assistant", Content: "an answer", RenderedContent: "an answer"},
	}
	// Keep an accent-owning cached child visible. A plain settled transcript is
	// allowed to use only text/background roles, so it cannot prove that the
	// selected theme's accent reached child surfaces.
	m.openThemePicker()
	m.refreshTranscript()
	before := m.View().Content

	if !m.SetTheme("dracula") {
		t.Fatal("SetTheme rejected a registered theme")
	}
	m.refreshTranscript()
	after := m.View().Content

	if before == after {
		t.Fatal("theme switch did not change any painted cell")
	}
	// The Dracula dark accent must actually appear somewhere in the frame.
	if !strings.Contains(after, "139;233;253") { // #8BE9FD
		t.Fatalf("selected theme's accent is absent from the frame:\n%q", after[:min(600, len(after))])
	}
	if m.md == nil || m.md.themeID != "dracula" {
		t.Fatal("markdown renderer kept the previous theme")
	}
}

func TestThemeSwitchRejectsUnknownAndKeepsCurrent(t *testing.T) {
	m := newTestModel(t)
	if !m.SetTheme("gruvbox") {
		t.Fatal("SetTheme rejected gruvbox")
	}
	if m.SetTheme("not-a-theme") {
		t.Fatal("SetTheme accepted an unregistered id")
	}
	if got := m.ThemeID(); got != "gruvbox" {
		t.Fatalf("rejected switch changed the active theme to %q", got)
	}
}

// No surface may keep painting in the default scheme once another is selected.
//
// This is the failure the non-variadic palette signatures exist to prevent, and
// it shipped anyway: eight call sites — including the constructors of the diff
// viewer, goal form, goal inspector and goal recovery — resolved a palette
// without a theme, so those panels opened in Nord while the rest of the frame
// used the selected scheme. The omission is invisible at the call site, so the
// frame itself is what has to be checked.
func TestNoSurfaceKeepsTheDefaultPaletteAfterASwitch(t *testing.T) {
	// Nord's dark accent. If it survives anywhere in a Dracula frame, some
	// surface resolved its colors without the active theme.
	const nordAccent = "136;192;208" // #88C0D0

	for _, overlay := range []struct {
		name string
		open func(m *Model)
	}{
		{"transcript", func(*Model) {}},
		{"settings", (*Model).openSettingsPicker},
		{"theme picker", (*Model).openThemePicker},
		{"help", func(m *Model) { m.overlay = OverlayHelp; m.initHelpViewport() }},
		{"runtime", (*Model).openRuntimeStatus},
	} {
		t.Run(overlay.name, func(t *testing.T) {
			m := newTestModel(t)
			m.entries = []ChatEntry{
				{Kind: "user", Content: "hey"},
				{Kind: "assistant", Content: "an answer", RenderedContent: "an answer"},
			}
			if !m.SetTheme("dracula") {
				t.Fatal("SetTheme rejected dracula")
			}
			overlay.open(m)
			m.recalcViewportHeight()
			m.refreshTranscript()

			if view := m.View().Content; strings.Contains(view, nordAccent) {
				t.Fatalf("%s still paints with the default theme's accent", overlay.name)
			}
		})
	}
}
