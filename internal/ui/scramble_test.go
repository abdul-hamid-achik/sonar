package ui

import (
	"testing"
)

func TestNewScrambleModel(t *testing.T) {
	s := NewScrambleModel(true)

	if s.visible != 0 {
		t.Errorf("expected visible=0, got %d", s.visible)
	}
	if len(s.chars) != scrambleWidth {
		t.Errorf("expected %d chars, got %d", scrambleWidth, len(s.chars))
	}
	if s.id != 1 {
		t.Errorf("expected id=1, got %d", s.id)
	}
	if s.rng == nil {
		t.Error("expected rng to be initialized")
	}
}

func TestScrambleUpdate(t *testing.T) {
	s := NewScrambleModel(true)

	// Matching tick should advance visible
	tick := ScrambleTickMsg{ID: s.id, Frame: s.frame}
	s2, cmd := s.Update(tick)
	if s2.visible != 1 {
		t.Errorf("expected visible=1 after tick, got %d", s2.visible)
	}
	if cmd == nil {
		t.Error("expected non-nil cmd after matching tick")
	}

	// Stale tick (wrong ID) should be ignored
	staleTick := ScrambleTickMsg{ID: s.id + 999, Frame: s.frame}
	s3, cmd := s2.Update(staleTick)
	if s3.visible != s2.visible {
		t.Errorf("expected visible unchanged after stale tick, got %d", s3.visible)
	}
	if cmd != nil {
		t.Error("expected nil cmd after stale tick")
	}
}

func TestScrambleView(t *testing.T) {
	s := NewScrambleModel(true)

	// Empty at visible=0
	if v := s.View(); v != "" {
		t.Errorf("expected empty view at visible=0, got %q", v)
	}

	// After ticks, should produce non-empty output
	tick := ScrambleTickMsg{ID: s.id, Frame: s.frame}
	s, _ = s.Update(tick)
	s, _ = s.Update(ScrambleTickMsg{ID: s.id, Frame: s.frame})
	if v := s.View(); v == "" {
		t.Error("expected non-empty view after ticks")
	}
}

func TestScrambleReset(t *testing.T) {
	s := NewScrambleModel(true)

	// Advance some ticks
	tick := ScrambleTickMsg{ID: s.id, Frame: s.frame}
	s, _ = s.Update(tick)
	s, _ = s.Update(ScrambleTickMsg{ID: s.id, Frame: s.frame})

	oldID := s.id
	s.Reset()

	if s.visible != 0 {
		t.Errorf("expected visible=0 after reset, got %d", s.visible)
	}
	if s.id <= oldID {
		t.Errorf("expected id to increment after reset, got %d (was %d)", s.id, oldID)
	}
}

func TestScrambleRejectsDuplicateFrameTicks(t *testing.T) {
	s := NewScrambleModel(true)
	tick := ScrambleTickMsg{ID: s.id, Frame: s.frame}

	advanced, next := s.Update(tick)
	if next == nil || advanced.frame != 1 {
		t.Fatalf("first frame did not advance: frame=%d cmd=%v", advanced.frame, next != nil)
	}
	duplicate, duplicateCmd := advanced.Update(tick)
	if duplicateCmd != nil || duplicate.frame != advanced.frame || duplicate.visible != advanced.visible {
		t.Fatalf("duplicate frame created a second clock: before=%#v after=%#v cmd=%v", advanced, duplicate, duplicateCmd != nil)
	}
}

func TestScrambleSetDark(t *testing.T) {
	s := NewScrambleModel(true)

	// Store dark colors
	darkFrom := s.colorFrom
	darkTo := s.colorTo

	// Switch to light
	s.SetDark(false)
	if s.colorFrom == darkFrom {
		t.Error("expected colorFrom to change for light theme")
	}
	if s.colorTo == darkTo {
		t.Error("expected colorTo to change for light theme")
	}
	// Switch back to dark
	s.SetDark(true)
	if s.colorFrom != darkFrom {
		t.Error("expected colorFrom to match original dark theme")
	}
	if s.colorTo != darkTo {
		t.Error("expected colorTo to match original dark theme")
	}
}
