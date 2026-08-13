package ui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func TestLastAssistantContent(t *testing.T) {
	t.Run("found", func(t *testing.T) {
		m := newTestModel(t)
		m.entries = []ChatEntry{
			{Kind: "user", Content: "hello"},
			{Kind: "assistant", Content: "world"},
		}
		got := m.lastAssistantContent()
		if got != "world" {
			t.Errorf("expected 'world', got %q", got)
		}
	})

	t.Run("not_found", func(t *testing.T) {
		m := newTestModel(t)
		m.entries = []ChatEntry{
			{Kind: "user", Content: "hello"},
			{Kind: "system", Content: "info"},
		}
		got := m.lastAssistantContent()
		if got != "" {
			t.Errorf("expected empty string, got %q", got)
		}
	})

	t.Run("returns_last", func(t *testing.T) {
		m := newTestModel(t)
		m.entries = []ChatEntry{
			{Kind: "assistant", Content: "first"},
			{Kind: "user", Content: "question"},
			{Kind: "assistant", Content: "second"},
		}
		got := m.lastAssistantContent()
		if got != "second" {
			t.Errorf("expected 'second', got %q", got)
		}
	})

	t.Run("empty_entries", func(t *testing.T) {
		m := newTestModel(t)
		m.entries = nil
		got := m.lastAssistantContent()
		if got != "" {
			t.Errorf("expected empty string, got %q", got)
		}
	})
}

func TestCopyLast_OnlyWhenIdleAndEmpty(t *testing.T) {
	t.Run("idle_empty_with_assistant", func(t *testing.T) {
		m := newTestModel(t)
		m.state = StateIdle
		m.entries = []ChatEntry{
			{Kind: "assistant", Content: "response text"},
		}
		m.input.SetValue("")

		_, cmd := m.Update(ctrlKey('y'))
		if cmd == nil {
			t.Error("expected a command to be returned for copy")
		}
	})

	t.Run("non_empty_input_no_trigger", func(t *testing.T) {
		m := newTestModel(t)
		m.state = StateIdle
		m.entries = []ChatEntry{
			{Kind: "assistant", Content: "response text"},
		}
		m.input.SetValue("some text")

		_, cmd := m.Update(ctrlKey('y'))
		// When input is non-empty, ctrl+y should not trigger copy.
		// The cmd may be non-nil (textarea update), but no copy should occur.
		// Verify no system message about clipboard appears.
		if cmd != nil {
			msg := cmd()
			if sysMsg, ok := msg.(SystemMessageMsg); ok {
				if sysMsg.Msg == "Copied to clipboard." {
					t.Error("should not trigger copy when input is non-empty")
				}
			}
		}
	})

	t.Run("streaming_empty_draft_can_copy", func(t *testing.T) {
		// Ctrl+Y is allowed while a turn is live when the draft is empty so
		// operators can grab text without waiting for idle.
		m := newTestModel(t)
		m.state = StateStreaming
		m.entries = []ChatEntry{
			{Kind: "assistant", Content: "response text"},
		}
		m.input.SetValue("")
		copied := ""
		m.clipboardWrite = func(value string) error {
			copied = value
			return nil
		}
		_, cmd := m.Update(ctrlKey('y'))
		if cmd == nil {
			t.Fatal("expected copy command while streaming with empty draft")
		}
		result := awaitCommandMessage[clipboardResultMsg](t, commandMessages(cmd), time.Second)
		if result.Err != nil {
			t.Fatalf("copy err: %v", result.Err)
		}
		if copied != "response text" && !strings.Contains(copied, "response") {
			t.Fatalf("clipboard content = %q", copied)
		}
	})

	t.Run("no_assistant_entries", func(t *testing.T) {
		m := newTestModel(t)
		m.state = StateIdle
		m.entries = []ChatEntry{
			{Kind: "user", Content: "hello"},
		}
		m.input.SetValue("")

		_, cmd := m.Update(ctrlKey('y'))
		// Should not return a copy command when there's no assistant content
		if cmd != nil {
			msg := cmd()
			if sysMsg, ok := msg.(SystemMessageMsg); ok {
				if sysMsg.Msg == "Copied to clipboard." {
					t.Error("should not trigger copy when no assistant content")
				}
			}
		}
	})
}

func TestCopyLastReceiptIsTransientAndDoesNotPolluteTranscript(t *testing.T) {
	m := newTestModel(t)
	m.entries = []ChatEntry{{Kind: "assistant", Content: "response text"}}
	m.input.SetValue("")
	var copied string
	m.clipboardWrite = func(value string) error {
		copied = value
		return nil
	}
	before := len(m.entries)

	updated, command := m.Update(ctrlKey('y'))
	m = updated.(*Model)
	result := awaitCommandMessage[clipboardResultMsg](t, commandMessages(command), time.Second)
	updated, _ = m.Update(result)
	m = updated.(*Model)

	if copied != "response text" {
		t.Fatalf("clipboard content = %q", copied)
	}
	if len(m.entries) != before {
		t.Fatalf("copy mutated transcript: entries=%d, want %d", len(m.entries), before)
	}
	if m.footerNotice == nil || m.footerNotice.text != "Copied to clipboard." {
		t.Fatalf("copy footer receipt = %#v", m.footerNotice)
	}
}

func TestCopyLastEmitsOSC52AndHostClipboard(t *testing.T) {
	// SSH/tmux have no pbcopy. The host write is best-effort; OSC 52 is the
	// sequence the terminal actually honours. Both must leave on every copy.
	m := newTestModel(t)
	m.entries = []ChatEntry{{Kind: "assistant", Content: "response text"}}
	m.input.SetValue("")
	var copied string
	m.clipboardWrite = func(value string) error {
		copied = value
		return nil
	}
	_, cmd := m.Update(ctrlKey('y'))
	if cmd == nil {
		t.Fatal("expected copy command")
	}
	msg := cmd()
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		t.Fatalf("copy cmd = %T, want BatchMsg (OSC 52 + host write)", msg)
	}
	if len(batch) != 2 {
		t.Fatalf("copy batch len = %d, want 2", len(batch))
	}
	result := awaitCommandMessage[clipboardResultMsg](t, commandMessages(cmd), time.Second)
	if result.Err != nil {
		t.Fatalf("copy err: %v", result.Err)
	}
	if copied != "response text" {
		t.Fatalf("host clipboard = %q", copied)
	}
}
