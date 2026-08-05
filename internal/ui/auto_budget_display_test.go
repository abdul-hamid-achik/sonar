package ui

import (
	"strings"
	"testing"
	"time"
)

// The AUTO continuation row said "checkpoint 3/8" and nothing else. That was
// enough while the whole-turn ceiling was a fixed 8 segments and 90 minutes.
// Both are configurable now and a run can be given hours, so "checkpoint 3/40"
// says almost nothing — the wall clock ends a long job first, and the row
// carried no time at all.
func TestAutoContinuationShowsBothBudgetDimensions(t *testing.T) {
	m := newTestModel(t)
	started := time.Date(2026, 8, 5, 9, 0, 0, 0, time.UTC)
	m.autoCheckpoints.reset("turn-long", started, 40, 6*time.Hour)
	m.autoCheckpoints.segmentsContinued = 13
	m.now = func() time.Time { return started.Add(2*time.Hour + 30*time.Minute) }

	detail := m.autoContinuationBudgetDetail()
	for _, want := range []string{"checkpoint 13/40", "2h30m", "6h"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail %q is missing %q", detail, want)
		}
	}
}

// A ceiling is only useful next to what has been spent, so both sides must
// survive the default configuration too.
func TestDefaultCeilingStillReportsTime(t *testing.T) {
	m := newTestModel(t)
	started := time.Date(2026, 8, 5, 9, 0, 0, 0, time.UTC)
	m.autoCheckpoints.reset("turn", started, 0, 0)
	m.autoCheckpoints.segmentsContinued = 2
	m.now = func() time.Time { return started.Add(12 * time.Minute) }

	detail := m.autoContinuationBudgetDetail()
	if !strings.Contains(detail, "12m/1h30m") {
		t.Errorf("detail %q does not report elapsed against the default ceiling", detail)
	}
}

// A supervisor with no start time cannot report elapsed, and inventing a zero
// would read as a run that just began. The segment counter stands alone.
func TestMissingStartTimeReportsSegmentsOnly(t *testing.T) {
	m := newTestModel(t)
	m.autoCheckpoints.segmentsContinued = 1
	detail := m.autoContinuationBudgetDetail()
	if strings.Contains(detail, "/") != true {
		t.Fatalf("detail lost the segment counter: %q", detail)
	}
	if strings.Contains(detail, "0s") {
		t.Errorf("an unknown start time was reported as zero elapsed: %q", detail)
	}
}

// The span format has to stay readable across the whole configurable range: a
// six-hour ceiling must not read as "360m", and a two-minute run must not read
// as "0h".
func TestBudgetSpanPicksTheRightUnit(t *testing.T) {
	for _, test := range []struct {
		in   time.Duration
		want string
	}{
		{0, "0s"},
		{45 * time.Second, "45s"},
		{time.Minute, "1m"},
		{90 * time.Minute, "1h30m"},
		{6 * time.Hour, "6h"},
		{24 * time.Hour, "24h"},
		{2*time.Hour + 5*time.Minute, "2h05m"},
		{-time.Minute, "0s"},
	} {
		if got := formatBudgetSpan(test.in); got != test.want {
			t.Errorf("formatBudgetSpan(%v) = %q, want %q", test.in, got, test.want)
		}
	}
}
