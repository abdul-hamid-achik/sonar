package ui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// TestSonarPulseIsOneCellInEveryFrame is the constraint that makes the pulse
// adoptable at all. The activity rail budgets exactly one accent cell, so a
// two-cell frame would not look wrong — it would shift the whole row and steal
// the esc/queue affordance at the 30-column tier.
func TestSonarPulseIsOneCellInEveryFrame(t *testing.T) {
	m := newTestModel(t)
	seen := make(map[string]struct{}, sonarPulseFrames)
	for frame := range sonarPulseFrames {
		m.sonarPulseFrame = uint64(frame)
		glyph := m.sonarPulseGlyph()
		if glyph == "" {
			t.Fatalf("frame %d produced no glyph", frame)
		}
		if width := lipgloss.Width(glyph); width != 1 {
			t.Fatalf("frame %d is %d cells wide: %q", frame, width, ansi.Strip(glyph))
		}
		seen[ansi.Strip(glyph)] = struct{}{}
	}
	// Shape carries the pulse, so EVERY beat has to differ without its color.
	// A repeated shape at a dimmer color reads as a stutter on a monochrome
	// terminal, which is precisely where shape is doing all the work.
	if len(seen) != sonarPulseFrames {
		t.Fatalf("the pulse repeats a shape and is partly color: distinct shapes = %v", seen)
	}
}

func TestSonarPulseYieldsToReducedMotionAndASCII(t *testing.T) {
	reduced := newTestModel(t)
	reduced.reducedMotion = true
	if got := reduced.sonarPulseGlyph(); got != "" {
		t.Fatalf("reduced motion still animates: %q", got)
	}

	ascii := newTestModel(t)
	ascii.glyphProfile = GlyphASCII
	if got := ascii.sonarPulseGlyph(); got != "" {
		t.Fatalf("the ASCII profile imported a Unicode pulse: %q", got)
	}
	// And the counter must not advance under reduced motion, or a later
	// change of setting would resume mid-sequence from a phase nobody saw.
	before := reduced.sonarPulseFrame
	reduced.advanceSonarPulse()
	if reduced.sonarPulseFrame != before {
		t.Fatal("reduced motion advanced the pulse counter")
	}
}

// TestSonarPulseDrivesRailAndCardTogether pins the thing two animations in one
// frame get wrong. The rail and a running receipt share one clock; drawing them
// from two vocabularies would read as two unrelated things happening.
func TestSonarPulseDrivesRailAndCardTogether(t *testing.T) {
	m := newTestModel(t)
	m.state = StateStreaming
	m.toolsPending = 1
	for frame := range sonarPulseFrames {
		m.sonarPulseFrame = uint64(frame)
		rail := ansi.Strip(m.renderWorkingLine())
		card := ansi.Strip(m.runningToolActivityGlyph())
		if card == "" {
			t.Fatalf("frame %d: the running card lost its glyph", frame)
		}
		if !strings.Contains(rail, card) {
			t.Fatalf("frame %d: rail %q does not carry the card's glyph %q", frame, rail, card)
		}
	}
}

// The waiting phase keeps its informative trace. Identity is the weaker thing
// to spend the cells on where a fact exists to show.
func TestWaitingPhaseKeepsTheInformativeTrace(t *testing.T) {
	m := newTestModel(t)
	m.state = StateWaiting
	activity, ok := m.currentWorkingActivity()
	if !ok || !activity.waiting {
		t.Fatalf("waiting phase did not report a waiting activity: %#v ok=%v", activity, ok)
	}
	// needsScramble is what routes the waiting phase to the trace/scramble
	// branch rather than to the pulse.
	if !m.needsScramble() {
		t.Fatal("the waiting phase no longer owns the scramble/trace branch")
	}
}

// TestSonarPulseBeatsOncePerSecond pins the cadence, because the first version
// rode the spinner's clock directly and pinged four times a second. Nothing
// pings four times a second; it read as a vibration rather than as sonar,
// which is the entire reason the glyphs exist.
func TestSonarPulseBeatsOncePerSecond(t *testing.T) {
	m := newTestModel(t)
	start := m.sonarPulseFrame
	// MiniDot, the activity spinner, runs at 12fps.
	const activityTicksPerSecond = 12
	for range activityTicksPerSecond {
		m.advanceSonarPulse()
	}
	advanced := m.sonarPulseFrame - start
	if advanced != sonarPulseFrames {
		t.Fatalf("one second of ticks advanced %d beats, want one full cycle of %d",
			advanced, sonarPulseFrames)
	}
}

// TestSonarPulseBeatsShareTheTextBaseline is an optical constraint a width
// check cannot see. Geometric shapes (● U+25CF, ○ U+25CB) and bullets both
// measure one cell; only the bullets are drawn to share a baseline and an
// x-height with the words beside them. The first version used the geometric
// pair and looked pasted into the row.
func TestSonarPulseBeatsShareTheTextBaseline(t *testing.T) {
	geometric := map[string]string{"●": "U+25CF", "○": "U+25CB", "◉": "U+25C9", "◌": "U+25CC"}
	for _, beat := range sonarPulseBeats {
		if name, isGeometric := geometric[beat]; isGeometric {
			t.Errorf("beat %q (%s) is a geometric shape; inline animation uses the bullet family", beat, name)
		}
	}
}
