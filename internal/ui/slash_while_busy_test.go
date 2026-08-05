package ui

import (
	"context"
	"errors"
	"testing"
)

func TestSlashCommandRunsImmediatelyWhileTurnRuns(t *testing.T) {
	m := newTestModel(t)
	m.state = StateStreaming
	m.input.SetValue("/theme dracula")

	updated, _ := m.Update(enterKey())
	m = updated.(*Model)

	if got := m.ThemeID(); got != "dracula" {
		t.Fatalf("safe slash command did not run while running: ThemeID=%q", got)
	}
	if m.queuedFollowUp != nil {
		t.Fatalf("safe slash command consumed the follow-up queue: %#v", m.queuedFollowUp)
	}
	if got := m.input.Value(); got != "" {
		t.Fatalf("safe slash command left the draft in the composer: %q", got)
	}
}

func TestSafeSlashCommandRunsWhileAnotherFollowUpIsQueued(t *testing.T) {
	m := newTestModel(t)
	m.state = StateStreaming
	m.queuedFollowUp = &queuedFollowUp{Prompt: "already queued"}
	m.input.SetValue("/theme dracula")

	updated, _ := m.Update(enterKey())
	m = updated.(*Model)

	if got := m.ThemeID(); got != "dracula" {
		t.Fatalf("safe slash command did not run while a follow-up was queued: ThemeID=%q", got)
	}
	if m.queuedFollowUp == nil || m.queuedFollowUp.Prompt != "already queued" {
		t.Fatalf("safe slash command disturbed the queued follow-up: %#v", m.queuedFollowUp)
	}
}

func TestUnsafeSlashCommandDefersWhileTurnRuns(t *testing.T) {
	m := newTestModel(t)
	m.entries = []ChatEntry{{Kind: "user", Content: "prior turn"}}
	m.state = StateStreaming
	m.input.SetValue("/clear")

	updated, _ := m.Update(enterKey())
	m = updated.(*Model)

	if m.queuedFollowUp == nil || m.queuedFollowUp.Prompt != "/clear" {
		t.Fatalf("unsafe slash command was not queued: %#v", m.queuedFollowUp)
	}
	if len(m.entries) != 1 || m.entries[0].Content != "prior turn" {
		t.Fatalf("unsafe slash command executed while running: %#v", m.entries)
	}
}

func TestUnknownSlashCommandDefersWhileTurnRuns(t *testing.T) {
	m := newTestModel(t)
	m.state = StateStreaming
	m.input.SetValue("/definitely-not-a-command")

	updated, _ := m.Update(enterKey())
	m = updated.(*Model)

	if m.queuedFollowUp == nil || m.queuedFollowUp.Prompt != "/definitely-not-a-command" {
		t.Fatalf("unknown slash command was not queued: %#v", m.queuedFollowUp)
	}
}

func TestPlainPromptStillQueuesWhileTurnRuns(t *testing.T) {
	m := newTestModel(t)
	m.state = StateStreaming
	m.input.SetValue("check the tests after this")

	updated, _ := m.Update(enterKey())
	m = updated.(*Model)

	if m.queuedFollowUp == nil || m.queuedFollowUp.Prompt != "check the tests after this" {
		t.Fatalf("plain prompt was not queued: %#v", m.queuedFollowUp)
	}
}

func TestQueuedSlashCommandExecutesAfterSettledTurn(t *testing.T) {
	m := newTestModel(t)
	m.entries = []ChatEntry{{Kind: "user", Content: "prior turn"}}
	m.state = StateStreaming
	_, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.input.SetValue("/clear")

	updated, _ := m.Update(enterKey())
	m = updated.(*Model)
	if m.queuedFollowUp == nil {
		t.Fatal("precondition: /clear was not queued while running")
	}

	updated, cmd := m.Update(AgentDoneMsg{TurnID: "turn-first"})
	m = updated.(*Model)
	if cmd == nil {
		t.Fatal("settled turn did not schedule queued follow-up dispatch")
	}
	if m.queuedFollowUp != nil {
		t.Fatalf("queued /clear left the queue occupied: %#v", m.queuedFollowUp)
	}
	if len(m.entries) != 1 || m.entries[0].Kind != "system" {
		t.Fatalf("queued /clear did not replace the conversation after settle: %#v", m.entries)
	}
}

func TestFailedTurnRestoresQueuedSlashDraft(t *testing.T) {
	m := newTestModel(t)
	m.state = StateStreaming
	_, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.input.SetValue("/clear")

	updated, _ := m.Update(enterKey())
	m = updated.(*Model)
	if m.queuedFollowUp == nil {
		t.Fatal("precondition: /clear was not queued while running")
	}

	updated, _ = m.Update(AgentDoneMsg{TurnID: "turn-failed", Err: errors.New("provider failed")})
	m = updated.(*Model)
	if m.queuedFollowUp != nil {
		t.Fatalf("failed turn kept the queued command: %#v", m.queuedFollowUp)
	}
	if got := m.input.Value(); got != "/clear" {
		t.Fatalf("failed turn restored draft = %q, want /clear", got)
	}
}
