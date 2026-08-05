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

	trace := ansi.Strip(m.sonarWaitTrace(sonarTraceCells))
	if trace != "●··│··" {
		t.Fatalf("early trace = %q, want %q", trace, "●··│··")
	}

	// At the baseline the head covers the marker.
	m.chromeSpring.ping.waitStart = base.Add(-2 * time.Second)
	trace = ansi.Strip(m.sonarWaitTrace(sonarTraceCells))
	if trace != "···●··" {
		t.Fatalf("on-time trace = %q, want %q", trace, "···●··")
	}

	// Overdue: head pinned at the right edge, marker visible again.
	m.chromeSpring.ping.waitStart = base.Add(-10 * time.Second)
	trace = ansi.Strip(m.sonarWaitTrace(sonarTraceCells))
	if trace != "···│·●" {
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
