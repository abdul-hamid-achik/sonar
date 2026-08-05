package ui

import "testing"

// On a very wide terminal the conversation stopped well short of the right
// edge while the top bar, code fences and diffs used all of it. That is
// deliberate — long lines are harder to scan — but it is a judgement about
// typography, not a fact about the reader's terminal, and on an ultrawide
// display it reads as a dead margin.
//
// ui.prose_width makes it the reader's call. These pin that the knob works in
// both directions and that the default is unchanged.
func TestProseCapIsConfigurableInBothDirections(t *testing.T) {
	original := proseCap
	t.Cleanup(func() { proseCap = original })

	const wideTerminal = 240

	// Zero is the default and means "follow the pane", which is the whole
	// point: a measure pinned to a number nobody chose looked like a bug on
	// every terminal wider than it.
	SetProseCap(0)
	if got := proseWidthForWork(wideTerminal); got != wideTerminal {
		t.Errorf("default prose width = %d, want the pane's own %d", got, wideTerminal)
	}

	SetProseCap(220)
	if got := proseWidthForWork(wideTerminal); got != 220 {
		t.Errorf("configured prose width = %d, want the requested 220", got)
	}

	SetProseCap(80)
	if got := proseWidthForWork(wideTerminal); got != 80 {
		t.Errorf("lowered prose width = %d, want the configured 80", got)
	}
}

// A cap below the comfortable measure is still the reader's choice. Silently
// widening back out would make the setting look broken.
func TestATightCapIsHonoured(t *testing.T) {
	original := proseCap
	t.Cleanup(func() { proseCap = original })

	SetProseCap(50)
	if got := proseWidthForWork(200); got != 50 {
		t.Errorf("prose width = %d, want the configured 50", got)
	}
}

// The cap is a ceiling, never a floor: a narrow terminal keeps using all of
// itself rather than being padded out to the configured measure.
func TestANarrowTerminalIgnoresTheCap(t *testing.T) {
	original := proseCap
	t.Cleanup(func() { proseCap = original })

	SetProseCap(200)
	if got := proseWidthForWork(60); got != 60 {
		t.Errorf("narrow pane prose width = %d, want the pane's own 60", got)
	}
	if got := proseWidthForWork(0); got != 0 {
		t.Errorf("an empty pane produced %d", got)
	}
}
