package ui

import (
	"strings"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/agent"
)

func testContextBreakdown() agent.ContextBreakdown {
	return agent.ContextBreakdown{
		Model:       "qwen3.5:4b",
		NumCtx:      16384,
		TotalTokens: 4200,
		Sections: []agent.ContextSection{
			{Key: "system", Label: "System prompt", Tokens: 900},
			{Key: "environment", Label: "Environment", Tokens: 120},
			{Key: "tools", Label: "Tool schemas", Tokens: 2280, Detail: "3 MCP · 16 local"},
			{Key: "conversation", Label: "Conversation", Tokens: 900, Detail: "4 messages"},
		},
	}
}

func TestContextDoctorOverlayLifecycle(t *testing.T) {
	m := newTestModel(t)

	cmd := m.openContextDoctor()
	if cmd == nil {
		t.Fatal("openContextDoctor should return a measurement command")
	}
	if m.overlay != OverlayContextDoctor {
		t.Fatalf("overlay = %v, want OverlayContextDoctor", m.overlay)
	}
	if view := m.renderContextDoctor(); !strings.Contains(view, "Measuring") {
		t.Fatalf("pre-load view should show the measuring notice, got:\n%s", view)
	}

	tick := m.updateContextDoctorMessage(ContextDoctorMsg{
		RequestID: m.contextDoctorRequest,
		Breakdown: testContextBreakdown(),
	})
	if tick == nil {
		t.Fatal("loading a breakdown should start the spring animation")
		return
	}
	m.contextDoctorState.snap()

	view := m.renderContextDoctor()
	for _, want := range []string{"qwen3.5:4b", "Tool schemas", "Conversation", "█", "total", "free"} {
		if !strings.Contains(view, want) {
			t.Fatalf("rendered overlay missing %q:\n%s", want, view)
		}
	}

	updated, _ := m.Update(escKey())
	m = updated.(*Model)
	if m.overlay != OverlayNone {
		t.Fatalf("escape should dismiss the overlay, got %v", m.overlay)
	}
	if m.contextDoctorState != nil {
		t.Fatal("closing the overlay should release its state")
	}
}

func TestContextDoctorStaleResponseIgnored(t *testing.T) {
	m := newTestModel(t)
	if cmd := m.openContextDoctor(); cmd == nil {
		t.Fatal("openContextDoctor should return a measurement command")
	}
	if tick := m.updateContextDoctorMessage(ContextDoctorMsg{
		RequestID: m.contextDoctorRequest - 1,
		Breakdown: testContextBreakdown(),
	}); tick != nil {
		t.Fatal("a stale request ID must not start the animation")
	}
	if m.contextDoctorState.Loaded {
		t.Fatal("a stale request ID must not populate the overlay")
	}
}

func TestContextDoctorReducedMotionSnaps(t *testing.T) {
	m := newTestModel(t)
	m.reducedMotion = true
	if cmd := m.openContextDoctor(); cmd == nil {
		t.Fatal("openContextDoctor should return a measurement command")
	}
	if tick := m.updateContextDoctorMessage(ContextDoctorMsg{
		RequestID: m.contextDoctorRequest,
		Breakdown: testContextBreakdown(),
	}); tick != nil {
		t.Fatal("reduced motion must not schedule animation ticks")
	}
	state := m.contextDoctorState
	for i, pos := range state.pos {
		if pos != 1 {
			t.Fatalf("reduced motion should snap bar %d to 1, got %f", i, pos)
		}
	}
	view := m.renderContextDoctor()
	if !strings.Contains(view, "█") {
		t.Fatalf("snapped view should paint full bars:\n%s", view)
	}
}

func TestContextDoctorCascadeGatesLaterBars(t *testing.T) {
	state := newContextDoctorState()
	state.pos = make([]float64, 3)
	state.vel = make([]float64, 3)
	state.step()
	if state.pos[0] == 0 {
		t.Fatal("first bar should move on the first step")
	}
	if state.pos[1] != 0 || state.pos[2] != 0 {
		t.Fatalf("later bars should wait for the cascade, got %v", state.pos)
	}
	for i := 0; i < 600 && state.active(); i++ {
		state.step()
	}
	for i, pos := range state.pos {
		if pos < 0.99 {
			t.Fatalf("bar %d should settle near 1, got %f", i, pos)
		}
	}
}
