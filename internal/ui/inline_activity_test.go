package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestActiveTurnWithoutTokensRendersLivenessBesideTranscript(t *testing.T) {
	m := newTestModel(t)
	m.width = 180
	m.state = StateWaiting
	m.entries = []ChatEntry{{Kind: "user", Content: "inspect the repository"}}

	view := ansi.Strip(m.renderEntries())
	if !strings.Contains(view, "Waiting for model…") {
		t.Fatalf("tokenless active turn omitted inline liveness:\n%s", view)
	}
}

func TestInlineLivenessExplainsCompactionAndPermission(t *testing.T) {
	m := newTestModel(t)
	m.state = StateWaiting
	m.entries = []ChatEntry{{Kind: "user", Content: "continue"}}
	m.compactingContext = true
	if got := ansi.Strip(m.renderEntries()); !strings.Contains(got, "Preparing context…") {
		t.Fatalf("compaction liveness = %q", got)
	}

	m.pendingApproval = &ToolApprovalMsg{ToolName: "bash"}
	m.refreshTranscript()
	if got := ansi.Strip(m.renderEntries()); !strings.Contains(got, "Waiting for permission below…") {
		t.Fatalf("permission liveness = %q", got)
	}
}

func TestRunningToolCardOwnsInlineLiveness(t *testing.T) {
	m := newTestModel(t)
	m.state = StateStreaming
	m.toolsPending = 1
	m.entries = []ChatEntry{{Kind: "user", Content: "list files"}}

	if got := ansi.Strip(m.renderEntries()); strings.Contains(got, "Waiting for model") {
		t.Fatalf("tool-running transcript duplicated activity: %q", got)
	}
}

func TestCompactionLifecycleAlwaysReturnsUIToOrdinaryRun(t *testing.T) {
	m := newTestModel(t)
	m.state = StateWaiting

	updated, _ := m.Update(ContextCompactionStartedMsg{})
	m = updated.(*Model)
	if !m.compactingContext {
		t.Fatal("compaction start was not projected")
	}

	updated, _ = m.Update(ContextCompactionFinishedMsg{})
	m = updated.(*Model)
	if m.compactingContext {
		t.Fatal("compaction finish left stale UI activity")
	}
}
