package ui

import (
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/llm"
)

// /context must not call into ModelManager on the event loop. SetNumCtx takes
// the exclusive inference lock, which a background session-title or auto-memory
// stream holds — and Update is the goroutine that paints and reads keys, so the
// TUI froze with Ctrl+C unreachable for as long as that stream lasted.
func TestContextApplyIsDeferredToACommand(t *testing.T) {
	m := newTestModel(t)
	m.modelManager = llm.NewModelManager("http://127.0.0.1:1", 8192)

	text, apply, err := m.handleContextWindowCommand("set:16384")
	if err != nil {
		t.Fatalf("validation rejected a legal value: %v", err)
	}
	if apply == nil {
		t.Fatal("/context set applied synchronously instead of returning a command")
	}
	if text != "" {
		t.Fatalf("a receipt was emitted before the change was applied: %q", text)
	}

	// The command carries the work; running it yields the result message.
	msg, ok := apply().(numCtxAppliedMsg)
	if !ok {
		t.Fatalf("command returned %T, want numCtxAppliedMsg", apply())
	}
	if msg.Err != nil {
		t.Fatalf("apply failed: %v", msg.Err)
	}
	if msg.Value != 16384 {
		t.Fatalf("applied value = %d, want 16384", msg.Value)
	}
	if m.handleNumCtxApplied(msg); m.modelManager.ConfiguredNumCtx() != 16384 {
		t.Fatalf("num_ctx = %d after the result was handled", m.modelManager.ConfiguredNumCtx())
	}
}

// Status is pure reporting and must stay synchronous — deferring it would make
// a read-only query wait behind an unrelated inference stream.
func TestContextStatusStaysSynchronous(t *testing.T) {
	m := newTestModel(t)
	m.modelManager = llm.NewModelManager("http://127.0.0.1:1", 8192)

	text, apply, err := m.handleContextWindowCommand("status")
	if err != nil {
		t.Fatalf("status reported an error: %v", err)
	}
	if apply != nil {
		t.Fatal("status returned a command; it performs no mutation")
	}
	if text == "" {
		t.Fatal("status returned no report")
	}
}

// A superseded result must not repaint over a newer selection.
func TestStaleContextResultIsDropped(t *testing.T) {
	m := newTestModel(t)
	m.modelManager = llm.NewModelManager("http://127.0.0.1:1", 8192)

	_, first, err := m.handleContextWindowCommand("set:16384")
	if err != nil || first == nil {
		t.Fatalf("first apply not deferred: err=%v", err)
	}
	stale, _ := first().(numCtxAppliedMsg)

	if _, second, err := m.handleContextWindowCommand("set:32768"); err != nil || second == nil {
		t.Fatalf("second apply not deferred: err=%v", err)
	}

	before := len(m.entries)
	m.handleNumCtxApplied(stale)
	if len(m.entries) != before {
		t.Fatalf("a superseded result was recorded: %+v", m.entries[before:])
	}
}
