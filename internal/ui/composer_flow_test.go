package ui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/abdul-hamid-achik/sonar/internal/agent"
)

func TestComposerRemainsEditableDuringOrdinaryTurn(t *testing.T) {
	for _, test := range []struct {
		name  string
		state State
	}{
		{name: "waiting", state: StateWaiting},
		{name: "streaming", state: StateStreaming},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := newTestModel(t)
			m.state = test.state

			for _, char := range "next" {
				updated, _ := m.Update(charKey(char))
				m = updated.(*Model)
			}

			if got := m.input.Value(); got != "next" {
				t.Fatalf("running composer draft = %q, want next", got)
			}
			if view := ansi.Strip(m.View().Content); !strings.Contains(view, "next") {
				t.Fatalf("running view hid recoverable draft:\n%s", view)
			}
		})
	}
}

func TestRunningEmptyComposerExplainsFollowUpQueue(t *testing.T) {
	m := newTestModel(t)
	m.state = StateWaiting
	view := ansi.Strip(m.View().Content)
	// Full placeholder may truncate under tight paint budgets (long workspace
	// paths on CI). The live activity rail always surfaces "enter queue".
	if !strings.Contains(view, "enter queue") {
		t.Fatalf("running composer omitted queue guidance:\n%s", view)
	}
	if !strings.Contains(view, "Write a follow") {
		t.Fatalf("running composer omitted follow-up placeholder:\n%s", view)
	}
}

func TestComposerQueuesOneFollowUpAndShowsReceipt(t *testing.T) {
	m := newTestModel(t)
	m.state = StateStreaming
	m.input.SetValue("check the tests after this")

	updated, _ := m.Update(enterKey())
	m = updated.(*Model)
	if m.queuedFollowUp == nil || m.queuedFollowUp.Prompt != "check the tests after this" {
		t.Fatalf("queued follow-up = %#v", m.queuedFollowUp)
	}
	if got := m.input.Value(); got != "" {
		t.Fatalf("queued composer was not cleared: %q", got)
	}
	if m.composerEditable() {
		t.Fatal("composer accepted a hidden second follow-up")
	}
	updated, _ = m.Update(charKey('x'))
	m = updated.(*Model)
	if got := m.input.Value(); got != "" {
		t.Fatalf("queued slot accepted a second hidden draft: %q", got)
	}
	view := ansi.Strip(m.View().Content)
	for _, want := range []string{"queued", "check the tests after this", "↑ edit", "esc clear"} {
		if !strings.Contains(view, want) {
			t.Fatalf("visible queue receipt omitted %q:\n%s", want, view)
		}
	}
	if status := ansi.Strip(m.renderStatusLine()); strings.Contains(status, "follow-up queued") || strings.Contains(status, "check the tests") {
		t.Fatalf("working status duplicated the visible queue row: %q", status)
	}
}

func TestUpEditsQueuedFollowUpBeforeHistory(t *testing.T) {
	m := newTestModel(t)
	m.state = StateStreaming
	m.queuedFollowUp = &queuedFollowUp{Prompt: "revise the queued instruction"}
	m.pushHistory("older prompt")

	updated, _ := m.Update(upKey())
	m = updated.(*Model)
	if m.queuedFollowUp != nil {
		t.Fatalf("Up left queued follow-up hidden: %#v", m.queuedFollowUp)
	}
	if got := m.input.Value(); got != "revise the queued instruction" {
		t.Fatalf("Up restored composer = %q", got)
	}
	if m.historyIndex != -1 {
		t.Fatalf("Up navigated history before editing queue: index=%d", m.historyIndex)
	}
	if !m.composerEditable() {
		t.Fatal("edited queued follow-up did not return composer authority")
	}
}

func TestEscapeClearsQueueBeforeCancellingRun(t *testing.T) {
	m := newTestModel(t)
	m.state = StateStreaming
	m.queuedFollowUp = &queuedFollowUp{Prompt: "keep the active run"}
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel

	updated, _ := m.Update(escKey())
	m = updated.(*Model)
	if m.queuedFollowUp != nil {
		t.Fatalf("first Escape did not clear queue: %#v", m.queuedFollowUp)
	}
	select {
	case <-ctx.Done():
		t.Fatal("first Escape cancelled the run while clearing the queue")
	default:
	}

	_, _ = m.Update(escKey())
	select {
	case <-ctx.Done():
	default:
		t.Fatal("second Escape did not reach active-run cancellation")
	}
}

func TestQueuedFollowUpPreviewStaysOnOneBoundedRow(t *testing.T) {
	for _, width := range []int{30, 48, 100} {
		m := newTestModel(t)
		m.width = width
		m.state = StateStreaming
		m.queuedFollowUp = &queuedFollowUp{Prompt: strings.Repeat("inspect the focused behavior ", 10)}

		preview := ansi.Strip(m.renderQueuedFollowUp())
		if strings.Contains(preview, "\n") {
			t.Fatalf("width %d queue preview wrapped: %q", width, preview)
		}
		if got := lipgloss.Width(preview); got == 0 || got > m.chatPaneWidth() {
			t.Fatalf("width %d queue preview width = %d, pane = %d: %q", width, got, m.chatPaneWidth(), preview)
		}
		if !strings.Contains(preview, "queued") {
			t.Fatalf("width %d queue preview lost identity: %q", width, preview)
		}
	}
}

func TestSettledTurnKeepsUnqueuedDraftInComposer(t *testing.T) {
	m := newTestModel(t)
	m.state = StateStreaming
	m.input.SetValue("revise the next instruction")
	_, cancel := context.WithCancel(context.Background())
	m.cancel = cancel

	updated, _ := m.Update(AgentDoneMsg{TurnID: "turn-first"})
	m = updated.(*Model)
	if m.state != StateIdle || m.queuedFollowUp != nil {
		t.Fatalf("settled draft state = %v queue %#v", m.state, m.queuedFollowUp)
	}
	if got := m.input.Value(); got != "revise the next instruction" {
		t.Fatalf("settled turn changed unqueued draft to %q", got)
	}
}

func TestSettledTurnDispatchesQueuedFollowUp(t *testing.T) {
	m := newTestModel(t)
	m.state = StateStreaming
	m.queuedFollowUp = &queuedFollowUp{Prompt: "run the focused checks"}
	_, cancel := context.WithCancel(context.Background())
	m.cancel = cancel

	updated, cmd := m.Update(AgentDoneMsg{TurnID: "turn-first"})
	m = updated.(*Model)
	if cmd == nil {
		t.Fatal("settled turn did not schedule queued follow-up")
		return
	}
	if m.queuedFollowUp != nil || m.state != StateWaiting {
		t.Fatalf("queued dispatch state = queue %#v state %v", m.queuedFollowUp, m.state)
	}
	if got := m.input.Value(); got != "" {
		t.Fatalf("dispatched follow-up remained in composer: %q", got)
	}
	if len(m.entries) == 0 || m.entries[len(m.entries)-1].Kind != "user" || m.entries[len(m.entries)-1].Content != "run the focused checks" {
		t.Fatalf("queued follow-up was not presented as the next turn: %#v", m.entries)
	}
}

func TestFailedTurnRestoresQueuedFollowUpWithoutRetry(t *testing.T) {
	m := newTestModel(t)
	m.state = StateStreaming
	m.queuedFollowUp = &queuedFollowUp{Prompt: "revise this before retrying"}
	_, cancel := context.WithCancel(context.Background())
	m.cancel = cancel

	updated, _ := m.Update(AgentDoneMsg{TurnID: "turn-failed", Err: errors.New("provider failed")})
	m = updated.(*Model)
	if m.state != StateIdle || m.queuedFollowUp != nil {
		t.Fatalf("failed turn queue state = state %v queue %#v", m.state, m.queuedFollowUp)
	}
	if got := m.input.Value(); got != "revise this before retrying" {
		t.Fatalf("failed turn restored draft = %q", got)
	}
}

func TestCancelledTurnRestoresQueuedFollowUp(t *testing.T) {
	m := newTestModel(t)
	m.state = StateStreaming
	m.queuedFollowUp = &queuedFollowUp{Prompt: "keep this after cancellation"}
	_, cancel := context.WithCancel(context.Background())
	m.cancel = cancel

	updated, _ := m.Update(AgentDoneMsg{TurnID: "turn-cancelled", Err: context.Canceled})
	m = updated.(*Model)
	if m.queuedFollowUp != nil || m.input.Value() != "keep this after cancellation" {
		t.Fatalf("cancelled queue=%#v draft=%q", m.queuedFollowUp, m.input.Value())
	}
}

func TestUndurableSettlementRestoresQueuedFollowUp(t *testing.T) {
	m := newTestModel(t)
	store, _ := attachGoalTestSession(t, m)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	m.state = StateStreaming
	m.queuedFollowUp = &queuedFollowUp{Prompt: "do not strand this follow-up"}
	_, cancel := context.WithCancel(context.Background())
	m.cancel = cancel

	updated, _ := m.Update(AgentDoneMsg{TurnID: "turn-undurable"})
	m = updated.(*Model)
	if m.state != StateIdle || m.queuedFollowUp != nil {
		t.Fatalf("undurable settlement state=%v queue=%#v", m.state, m.queuedFollowUp)
	}
	if got := m.input.Value(); got != "do not strand this follow-up" {
		t.Fatalf("undurable settlement restored draft = %q", got)
	}
}

func TestGoalTurnDoesNotExposeOrdinaryFollowUpQueue(t *testing.T) {
	m := newTestModel(t)
	m.state = StateStreaming
	m.goalTurnID = "goal-turn"
	if m.composerEditable() {
		t.Fatal("goal continuation exposed an ordinary follow-up composer")
	}

	updated, _ := m.Update(charKey('x'))
	m = updated.(*Model)
	if got := m.input.Value(); got != "" {
		t.Fatalf("goal turn accepted hidden composer input: %q", got)
	}
}

func TestIterationLimitArmsEnterToContinue(t *testing.T) {
	m := newTestModel(t)
	m.state = StateStreaming

	_ = m.handleAgentDone(AgentDoneMsg{
		Err: fmt.Errorf("%w (10)", agent.ErrMaxIterations),
	}, nil)

	if !m.iterationLimitContinue {
		t.Fatal("iteration-limit turn did not arm the continue gesture")
	}
	found := false
	for _, entry := range m.entries {
		if entry.Kind == "system" && strings.Contains(entry.Content, "enter on the empty composer") {
			found = true
		}
	}
	if !found {
		t.Fatal("no system entry advertises the continue gesture")
	}

	// A cancelled turn must not arm it.
	m2 := newTestModel(t)
	m2.state = StateStreaming
	_ = m2.handleAgentDone(AgentDoneMsg{Err: context.Canceled}, nil)
	if m2.iterationLimitContinue {
		t.Fatal("cancelled turn armed the continue gesture")
	}
}
