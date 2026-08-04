package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/abdul-hamid-achik/sonar/internal/agent"
	"github.com/abdul-hamid-achik/sonar/internal/db"
	"github.com/abdul-hamid-achik/sonar/internal/execution"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
)

func TestHeadlessSessionTitleUsesFirstPromptLine(t *testing.T) {
	t.Parallel()

	if got := headlessSessionTitle("\n \t\n  inspect the release\nthen summarize  "); got != "inspect the release" {
		t.Fatalf("headlessSessionTitle() = %q", got)
	}
}

func TestHeadlessSessionTitleSanitizesTerminalControls(t *testing.T) {
	t.Parallel()

	prompt := "\x1b[31m  inspect\t\u202ethe release  \x1b[0m\nthen summarize"
	if got := headlessSessionTitle(prompt); got != "inspect the release" {
		t.Fatalf("headlessSessionTitle() = %q", got)
	}
}

func TestHeadlessSessionTitleBoundsUnicodeByRunes(t *testing.T) {
	t.Parallel()

	got := headlessSessionTitle(strings.Repeat("界", 80))
	if !utf8.ValidString(got) {
		t.Fatalf("headlessSessionTitle() returned invalid UTF-8: %q", got)
	}
	if count := len([]rune(got)); count != 72 {
		t.Fatalf("headlessSessionTitle() rune count = %d, want 72", count)
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("headlessSessionTitle() = %q, want ellipsis suffix", got)
	}
}

func TestHeadlessSessionTitleNamesBlankPrompt(t *testing.T) {
	t.Parallel()

	if got := headlessSessionTitle(" \n\t"); !strings.HasPrefix(got, "Headless session ") {
		t.Fatalf("headlessSessionTitle() = %q", got)
	}
}

func TestSaveHeadlessSessionStateCASChainsAndDoesNotRetryConflict(t *testing.T) {
	store, err := db.OpenPath(filepath.Join(t.TempDir(), "headless-cas.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	session, err := store.CreateSession(context.Background(), db.CreateSessionParams{
		Title: "headless CAS", WorkspaceID: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}

	revision := int64(0)
	revision, err = saveHeadlessSessionStateCAS(context.Background(), store, session.ID, revision, `{"turn":1}`)
	if err != nil || revision != 1 {
		t.Fatalf("first save revision/error = %d, %v", revision, err)
	}
	revision, err = saveHeadlessSessionStateCAS(context.Background(), store, session.ID, revision, `{"turn":2}`)
	if err != nil || revision != 2 {
		t.Fatalf("second save revision/error = %d, %v", revision, err)
	}

	external, err := store.SaveSessionStateCAS(context.Background(), session.ID, revision, `{"writer":"external"}`)
	if err != nil {
		t.Fatal(err)
	}
	staleRevision, err := saveHeadlessSessionStateCAS(
		context.Background(), store, session.ID, revision, `{"writer":"stale-headless"}`,
	)
	if !errors.Is(err, db.ErrSessionStateConflict) {
		t.Fatalf("stale save error = %v", err)
	}
	if staleRevision != revision {
		t.Fatalf("failed save advanced caller revision to %d, want %d", staleRevision, revision)
	}
	stored, err := store.GetSessionStateRecord(context.Background(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Revision != external.Revision || stored.StateJSON != `{"writer":"external"}` {
		t.Fatalf("stale save overwrote durable state: revision=%d state=%s", stored.Revision, stored.StateJSON)
	}
}

func TestHeadlessSnapshotCursorRequiresProjectedCompletedReceipt(t *testing.T) {
	store, err := db.OpenPath(filepath.Join(t.TempDir(), "headless.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	workspace := t.TempDir()
	session, err := store.CreateSession(context.Background(), db.CreateSessionParams{Title: "headless", WorkspaceID: workspace})
	if err != nil {
		t.Fatal(err)
	}
	identity := execution.Identity{
		SessionID: session.ID, WorkspaceID: workspace, RunID: "run_headless", TurnID: "turn_headless",
		ExecutionID: "exec_headless", IdempotencyKey: "idem_headless", CanonicalCallID: "call_headless",
		ToolName: "write", Iteration: 1, Ordinal: 1, Kind: execution.KindBuiltin, EffectClass: execution.Effectful,
	}
	requested := execution.Event{
		Identity: identity, Type: execution.EventRequested, Approval: execution.ApprovalNotApplicable,
		ArgumentsSHA256: execution.HashText("arguments"),
	}
	approved := requested
	approved.Type = execution.EventApproved
	approved.Approval = execution.ApprovalEmbedding
	started := requested
	started.Type = execution.EventStarted
	completed := requested
	completed.Type = execution.EventCompleted
	completed.ResultSHA256 = execution.HashText("done")
	for _, event := range []execution.Event{requested, approved, started} {
		if _, _, err := store.AppendExecutionEvent(context.Background(), event); err != nil {
			t.Fatal(err)
		}
	}
	terminal, _, err := store.AppendExecutionEvent(context.Background(), completed)
	if err != nil {
		t.Fatal(err)
	}

	missing := agent.New(nil, nil, 4096)
	if cursor, err := headlessSnapshotExecutionCursor(context.Background(), store, missing, session.ID, workspace, 0); err == nil || cursor != 0 {
		t.Fatalf("missing receipt cursor/error = %d, %v", cursor, err)
	}
	wrongResult := agent.New(nil, nil, 4096)
	wrongResult.AppendMessage(llm.Message{Role: "tool", ToolCallID: identity.CanonicalCallID, ToolName: identity.ToolName, Content: "wrong result"})
	if cursor, err := headlessSnapshotExecutionCursor(context.Background(), store, wrongResult, session.ID, workspace, 0); err == nil || cursor != 0 {
		t.Fatalf("wrong-result receipt cursor/error = %d, %v", cursor, err)
	}
	projected := agent.New(nil, nil, 4096)
	projected.AppendMessage(llm.Message{Role: "tool", ToolCallID: identity.CanonicalCallID, ToolName: identity.ToolName, Content: "done"})
	if cursor, err := headlessSnapshotExecutionCursor(context.Background(), store, projected, session.ID, workspace, 0); err != nil || cursor != terminal.ID {
		t.Fatalf("projected receipt cursor/error = %d, %v; want %d", cursor, err, terminal.ID)
	}
}
