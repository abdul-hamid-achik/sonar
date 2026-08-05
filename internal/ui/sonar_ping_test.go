package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// sonarTestModel is a Unicode, wide-tier model with a frozen clock the tests
// advance explicitly.
func sonarTestModel(t *testing.T, base time.Time) *Model {
	t.Helper()
	m := newTestModel(t)
	m.model = "deepseek-v4-flash"
	m.now = func() time.Time { return base }
	// Settle the model-identity observation so later transitions are measured
	// against a stable baseline record.
	m.observeSonarPing()
	return m
}

func TestSonarPingRecordsEchoThroughUpdate(t *testing.T) {
	base := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	m := sonarTestModel(t, base)

	m.state = StateWaiting
	updated, _ := m.Update(struct{}{})
	m = updated.(*Model)
	ping := m.chromeSpring.ping
	if !ping.inWait || !ping.waitStart.Equal(base) {
		t.Fatalf("waiting entry not observed: %+v", ping)
	}

	m.now = func() time.Time { return base.Add(1200 * time.Millisecond) }
	m.state = StateStreaming
	updated, _ = m.Update(struct{}{})
	m = updated.(*Model)
	ping = m.chromeSpring.ping
	if ping.inWait || ping.samples != 1 || ping.baseline != 1200*time.Millisecond ||
		ping.last != 1200*time.Millisecond {
		t.Fatalf("first echo not recorded: %+v", ping)
	}
}

func TestSonarPingBaselineIsAnEMAAndDiscardsUnechoedWaits(t *testing.T) {
	base := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	m := sonarTestModel(t, base)

	record := func(wait time.Duration, echoed bool) {
		start := m.nowTime()
		m.state = StateWaiting
		m.observeSonarPing()
		m.now = func() time.Time { return start.Add(wait) }
		if echoed {
			m.state = StateStreaming
		} else {
			m.state = StateIdle
		}
		m.observeSonarPing()
		m.state = StateIdle
		m.observeSonarPing()
	}

	record(2*time.Second, true)
	record(1*time.Second, true)
	ping := m.chromeSpring.ping
	// EMA with newest-quarter weight: (2s*3 + 1s) / 4 = 1.75s.
	if ping.samples != 2 || ping.baseline != 1750*time.Millisecond {
		t.Fatalf("EMA baseline = %+v, want samples=2 baseline=1.75s", ping)
	}

	// A cancelled wait never echoed; it must not move the baseline.
	record(30*time.Second, false)
	ping = m.chromeSpring.ping
	if ping.samples != 2 || ping.baseline != 1750*time.Millisecond {
		t.Fatalf("unechoed wait moved the baseline: %+v", ping)
	}
}

func TestSonarPingBaselineResetsOnModelSwitch(t *testing.T) {
	base := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	m := sonarTestModel(t, base)
	m.chromeSpring.ping.baseline = 2 * time.Second
	m.chromeSpring.ping.last = 2 * time.Second
	m.chromeSpring.ping.samples = 3

	m.model = "another-model"
	m.observeSonarPing()
	ping := m.chromeSpring.ping
	if ping.samples != 0 || ping.baseline != 0 || ping.lastModel != "another-model" {
		t.Fatalf("model switch did not reset the baseline: %+v", ping)
	}
}

func TestSonarTraceHeadMapsElapsedAgainstBaseline(t *testing.T) {
	baseline := 2 * time.Second
	tests := []struct {
		elapsed time.Duration
		head    int
		overdue bool
	}{
		{elapsed: 0, head: 0, overdue: false},
		{elapsed: 500 * time.Millisecond, head: 0, overdue: false},
		// At exactly the baseline the head sits on the expected-echo marker.
		{elapsed: 2 * time.Second, head: 3, overdue: false},
		{elapsed: 3 * time.Second, head: 4, overdue: false},
		// Late: pinned at the last cell, not yet the 2x warning.
		{elapsed: 3900 * time.Millisecond, head: 5, overdue: false},
		{elapsed: 4 * time.Second, head: 5, overdue: true},
		{elapsed: time.Minute, head: 5, overdue: true},
	}
	for _, tt := range tests {
		head, overdue := sonarTraceHead(tt.elapsed, baseline, sonarTraceCells)
		if head != tt.head || overdue != tt.overdue {
			t.Fatalf("sonarTraceHead(%v) = (%d, %v), want (%d, %v)",
				tt.elapsed, head, overdue, tt.head, tt.overdue)
		}
	}
}

func TestSonarWaitTraceRendersHeadMarkerAndTrack(t *testing.T) {
	base := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	m := sonarTestModel(t, base)
	m.state = StateWaiting
	m.turnStartedAt = base.Add(-500 * time.Millisecond)
	m.chromeSpring.ping.inWait = true
	m.chromeSpring.ping.waitStart = base.Add(-500 * time.Millisecond)
	m.chromeSpring.ping.baseline = 2 * time.Second
	m.chromeSpring.ping.last = 2 * time.Second
	m.chromeSpring.ping.samples = 1

	// The head follows the spring, not the arithmetic, so each position is
	// reached by settling rather than by assignment. That is the behaviour
	// change: on the first frame of a ping the head is still at the origin and
	// eases out, which is what makes it read as motion.
	settle := func() string {
		for range 400 {
			m.advanceSonarPing()
		}
		return ansi.Strip(m.sonarWaitTrace(sonarTraceCells))
	}

	// A quarter of the way to the baseline puts the true position at 0.75 of a
	// cell: the head renders on cell 1 and smears back onto cell 0. Two lit
	// cells is one head in flight, not two heads.
	trace := settle()
	if trace != "●●·│··" {
		t.Fatalf("early trace = %q, want %q", trace, "●●·│··")
	}

	// At the baseline the head covers the marker.
	m.chromeSpring.ping.waitStart = base.Add(-2 * time.Second)
	if trace = settle(); trace != "···●··" {
		t.Fatalf("on-time trace = %q, want %q", trace, "···●··")
	}

	// Overdue: head pinned at the right edge, marker visible again.
	m.chromeSpring.ping.waitStart = base.Add(-10 * time.Second)
	if trace = settle(); trace != "···│·●" {
		t.Fatalf("overdue trace = %q, want %q", trace, "···│·●")
	}

	// The trace appears in the activity rail at the wide tier.
	line := ansi.Strip(m.renderWorkingLine())
	if !strings.Contains(line, "···│·●") || !strings.Contains(line, "Running") {
		t.Fatalf("working line missing sonar trace:\n%s", line)
	}
}

func TestSonarWaitTraceStaysHonestWithoutABaseline(t *testing.T) {
	base := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	m := sonarTestModel(t, base)
	m.state = StateWaiting
	m.chromeSpring.ping.inWait = true
	m.chromeSpring.ping.waitStart = base.Add(-time.Second)

	if got := m.sonarWaitTrace(sonarTraceCells); got != "" {
		t.Fatalf("no-baseline trace = %q, want empty (scramble keeps the first wait)", got)
	}
	// Narrow tier: one motion cell cannot encode a position.
	m.chromeSpring.ping.baseline = 2 * time.Second
	m.chromeSpring.ping.samples = 1
	if got := m.sonarWaitTrace(1); got != "" {
		t.Fatalf("narrow trace = %q, want empty", got)
	}
	// ASCII profile: the waiting phase owns the spinner, not the trace.
	m.glyphProfile = GlyphASCII
	if got := m.sonarWaitTrace(sonarTraceCells); got != "" {
		t.Fatalf("ASCII trace = %q, want empty", got)
	}
}

func TestRuntimeStatusShowsEchoReceiptOnlyAfterAFirstResponse(t *testing.T) {
	base := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	m := sonarTestModel(t, base)

	before := ansi.Strip(m.buildRuntimeStatusContent(60))
	if strings.Contains(before, "Echo") {
		t.Fatalf("runtime shows an echo receipt before any first response:\n%s", before)
	}

	m.chromeSpring.ping.last = 1200 * time.Millisecond
	m.chromeSpring.ping.baseline = 1500 * time.Millisecond
	m.chromeSpring.ping.samples = 2
	after := ansi.Strip(m.buildRuntimeStatusContent(60))
	if !strings.Contains(after, "Echo") ||
		!strings.Contains(after, "last 1.2s") || !strings.Contains(after, "typical 1.5s") {
		t.Fatalf("runtime echo receipt missing latency facts:\n%s", after)
	}
}

// TestSonarWaitTraceReducedMotionSchedulesNoTicksAndRendersStaticFrame is the
// reduced-motion contract: no ticks from the observation hook, no animation
// clock ownership, and the frame is today's correct static form — ellipsis
// motion cell, live label, cancellation affordance — with no trace glyphs
// frozen mid-track.
func TestSonarWaitTraceReducedMotionSchedulesNoTicksAndRendersStaticFrame(t *testing.T) {
	base := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	m := sonarTestModel(t, base)
	m.reducedMotion = true
	m.state = StateWaiting
	m.turnStartedAt = base.Add(-3 * time.Second)
	m.chromeSpring.ping = sonarPingState{
		inWait:    true,
		waitStart: base.Add(-3 * time.Second),
		baseline:  1500 * time.Millisecond,
		last:      1500 * time.Millisecond,
		samples:   2,
		lastModel: m.model,
	}

	if got := m.sonarWaitTrace(sonarTraceCells); got != "" {
		t.Fatalf("reduced-motion trace = %q, want empty", got)
	}
	if m.needsScramble() || m.needsSpinner() {
		t.Fatal("reduced motion must not own an animation clock while waiting")
	}
	if cmd := m.maybeKickChromeSpring(); cmd != nil {
		t.Fatal("reduced-motion sonar observation scheduled a chrome spring tick")
	}

	first := ansi.Strip(m.renderWorkingLine())
	second := ansi.Strip(m.renderWorkingLine())
	if first != second {
		t.Fatalf("reduced-motion frame is not static:\n%q\n%q", first, second)
	}
	if !strings.Contains(first, "Running") || !strings.Contains(first, "esc") {
		t.Fatalf("reduced-motion frame lost the working grammar:\n%s", first)
	}
	if !strings.Contains(first, "…") {
		t.Fatalf("reduced-motion frame lost the static ellipsis motion cell:\n%s", first)
	}
	if strings.Contains(first, "●") || strings.Contains(first, "│") {
		t.Fatalf("reduced-motion frame contains frozen trace glyphs:\n%s", first)
	}
}

// The spring is the animation. Without it the head is a counter that teleports
// between six integer cells; with it the head has momentum, which is what the
// eye reads as movement. These pin the physics rather than the arithmetic.
func TestSonarPingSpringGivesTheHeadMomentum(t *testing.T) {
	base := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	m := sonarTestModel(t, base)
	m.state = StateWaiting
	m.chromeSpring.ping.inWait = true
	m.chromeSpring.ping.waitStart = base
	m.chromeSpring.ping.baseline = 2 * time.Second
	m.chromeSpring.ping.samples = 1

	// Hold elapsed at the baseline, so the target is the marker and stays
	// there. A spring must approach it over several frames.
	m.now = func() time.Time { return base.Add(2 * time.Second) }
	target := sonarTraceTarget(2*time.Second, 2*time.Second, sonarTraceCells)
	if target != float64(sonarTraceCells/2) {
		t.Fatalf("target at the baseline = %v, want the marker at %d", target, sonarTraceCells/2)
	}

	m.advanceSonarPing()
	first := m.chromeSpring.ping.headPos
	if first <= 0 {
		t.Fatalf("head did not leave the origin: %v", first)
	}
	if first >= target {
		t.Fatalf("head reached %v in a single frame; a spring eases in, it does not teleport", first)
	}

	// It keeps closing the distance frame over frame.
	m.advanceSonarPing()
	second := m.chromeSpring.ping.headPos
	if second <= first {
		t.Fatalf("head stalled: %v then %v", first, second)
	}

	// Under-damped: it overshoots the marker before settling. That frame is
	// where a wait visibly crosses from "normal" to "late".
	overshot := false
	for range 200 {
		m.advanceSonarPing()
		if m.chromeSpring.ping.headPos > target {
			overshot = true
			break
		}
	}
	if !overshot {
		t.Error("head never overshot the marker; the damping ratio is no longer under-damped")
	}
}

// A new request must start from the left edge at rest. Carrying the previous
// wait's velocity would fling the head across the track on the next turn.
func TestSonarPingResetsMomentumBetweenRequests(t *testing.T) {
	base := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	m := sonarTestModel(t, base)
	m.chromeSpring.ping.baseline = time.Second
	m.chromeSpring.ping.samples = 1
	m.chromeSpring.ping.headPos = 4.5
	m.chromeSpring.ping.headVel = 9

	m.state = StateWaiting
	m.observeSonarPing()
	if m.chromeSpring.ping.headPos != 0 || m.chromeSpring.ping.headVel != 0 {
		t.Fatalf("new request inherited motion: pos=%v vel=%v",
			m.chromeSpring.ping.headPos, m.chromeSpring.ping.headVel)
	}
}

// Reduced motion means no motion: the spring must not advance at all, so
// nothing is mid-flight if the setting is turned off later.
func TestSonarPingSpringIsInertUnderReducedMotion(t *testing.T) {
	base := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	m := sonarTestModel(t, base)
	m.reducedMotion = true
	m.state = StateWaiting
	m.chromeSpring.ping.inWait = true
	m.chromeSpring.ping.waitStart = base
	m.chromeSpring.ping.baseline = time.Second
	m.chromeSpring.ping.samples = 1
	m.now = func() time.Time { return base.Add(time.Second) }

	m.advanceSonarPing()
	if m.chromeSpring.ping.headPos != 0 || m.chromeSpring.ping.headVel != 0 {
		t.Errorf("reduced motion advanced the spring: pos=%v vel=%v",
			m.chromeSpring.ping.headPos, m.chromeSpring.ping.headVel)
	}
}

// The trailing echo is what makes six cells read as travel. It lights the cell
// being vacated in proportion to how far between cells the head sits, and
// nothing when the head is settled on a centre.
func TestSonarPingTrailLightsTheVacatedCell(t *testing.T) {
	if cell, weight := sonarTraceTrail(3.0, 3); cell != -1 || weight != 0 {
		t.Errorf("settled head produced a trail: cell=%d weight=%v", cell, weight)
	}
	// Past the rendered cell's centre: the head is between 3 and 4, so 4 is the
	// neighbour it is smearing into.
	cell, weight := sonarTraceTrail(3.4, 3)
	if cell != 4 {
		t.Errorf("trail for pos 3.4 = %d, want 4", cell)
	}
	if weight <= 0 || weight > 1 {
		t.Errorf("trail weight = %v, want within (0,1]", weight)
	}
	// Short of it: between 2 and 3, so 2.
	if cell, _ := sonarTraceTrail(2.6, 3); cell != 2 {
		t.Errorf("trail for pos 2.6 = %d, want 2", cell)
	}
	// Never off the ends. Short of cell 0 would smear onto -1.
	if cell, _ := sonarTraceTrail(-0.4, 0); cell != -1 {
		t.Errorf("trail ran off the left edge: %d", cell)
	}
	// Past the last cell would smear onto one that does not exist.
	last := sonarTraceCells - 1
	if cell, _ := sonarTraceTrail(float64(last)+0.4, last); cell != -1 {
		t.Errorf("trail ran off the right edge: %d", cell)
	}
	// Arriving at the last cell from the left still trails the one before it.
	if cell, _ := sonarTraceTrail(float64(last)-0.4, last); cell != last-1 {
		t.Errorf("trail into the right edge = %d, want %d", cell, last-1)
	}
}
