package ui

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/agent"
	"github.com/abdul-hamid-achik/sonar/internal/config"
	"github.com/abdul-hamid-achik/sonar/internal/db"
	"github.com/abdul-hamid-achik/sonar/internal/execution"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
)

// autoCheckpointProjectionFixture is one interactive AUTO turn that has already
// performed a non-read-only effect and is about to cross the iteration
// watchdog. Everything durable is real: SQLite session, held execution lease,
// and an append-only execution ledger the agent's strict pre-provider scan
// reads.
type autoCheckpointProjectionFixture struct {
	m           *Model
	store       *db.Store
	client      *standaloneRecoveryClient
	workspaceID string
	sessionID   int64
	callID      string
	result      string
}

func newAutoCheckpointProjectionFixture(t *testing.T) *autoCheckpointProjectionFixture {
	t.Helper()
	workspaceID, err := canonicalWorkspaceID(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store, err := db.OpenPath(filepath.Join(t.TempDir(), "auto-checkpoint-projection.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	session, err := store.CreateSession(context.Background(), db.CreateSessionParams{
		Title: "auto checkpoint projection", Model: "test", Mode: "AUTO", WorkspaceID: workspaceID,
	})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := store.AcquireExecutionSessionLease(context.Background(), session.ID, workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close() })

	client := &standaloneRecoveryClient{}
	m := newGoalRuntimeTestModel(t, client)
	m.model = client.Model()
	m.agent.SetWorkDir(workspaceID)
	m.agent.SetExecutionLedger(store)
	m.agent.SetExecutionSessionID(session.ID, session.PublicID)
	m.agent.SetExecutionSnapshotCursor(0)
	m.agent.RequireExecutionLedger(true)
	m.SetSessionStore(store)
	m.sessionID = session.ID
	m.sessionPublicID = session.PublicID
	m.executionLease = lease
	// A fresh session starts at cursor 0 — exactly the state in which the AUTO
	// watchdog fires for the first time.
	m.executionCursor = 0
	if err := m.initializeSessionStateRevision(0); err != nil {
		t.Fatal(err)
	}

	fixture := &autoCheckpointProjectionFixture{
		m: m, store: store, client: client,
		workspaceID: workspaceID, sessionID: session.ID,
		callID: "call_auto_segment", result: "wrote 3 lines to notes.md",
	}
	appendCompletedEffectfulExecution(t, store, session.ID, workspaceID,
		"turn_auto_segment_one", fixture.callID, fixture.result,
		time.Date(2026, time.July, 16, 11, 0, 0, 0, time.UTC))
	// The completed effect's receipt is already in the model transcript: the
	// Run that produced it settled every message before returning.
	m.agent.ReplaceMessages([]llm.Message{
		{Role: "user", Content: "do the long job"},
		{Role: "tool", ToolCallID: fixture.callID, Content: fixture.result},
	})
	m.entries = append(m.entries, ChatEntry{Kind: "user", Content: "do the long job"})
	if err := m.persistSessionState(context.Background()); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// armAutoSegment puts the fixture in the exact state the host holds while the
// first AUTO segment of a logical turn is streaming.
func (f *autoCheckpointProjectionFixture) armAutoSegment(started time.Time) {
	m := f.m
	m.now = func() time.Time { return started.Add(time.Minute) }
	m.state = StateStreaming
	m.turnStartedAt = started
	m.turnCheckpointSet = true
	m.turnLogicalID = "turn-root"
	m.turnSegmentID = "turn-root"
	m.turnAuthority = ModeAuto
	m.turnRunContext = context.Background()
	m.turnRunOptions = agent.TurnOptions{}
	m.autoCheckpoints.reset("turn-root", started, config.DefaultAutoMaxSegments, config.DefaultAutoMaxWallTime)
}

func appendCompletedEffectfulExecution(
	t *testing.T,
	store *db.Store,
	sessionID int64,
	workspaceID, turnID, canonicalCallID, result string,
	occurredAt time.Time,
) {
	t.Helper()
	base := execution.Event{
		Identity: execution.Identity{
			SessionID: sessionID, WorkspaceID: workspaceID,
			RunID: "run_auto_segment", TurnID: turnID,
			ExecutionID: "exec_auto_segment", IdempotencyKey: "idem_auto_segment",
			ProviderCallID: "provider_auto_segment", CanonicalCallID: canonicalCallID,
			ToolName: "write", Iteration: 1, Ordinal: 1,
			Kind: execution.KindBuiltin, EffectClass: execution.Effectful,
		},
		Type: execution.EventRequested, Approval: execution.ApprovalNotApplicable,
		ArgumentsSHA256: execution.HashText(`{"path":"notes.md"}`), OccurredAt: occurredAt,
	}
	events := []execution.Event{base}
	approved := base
	approved.Type = execution.EventApproved
	approved.Approval = execution.ApprovalPolicy
	approved.OccurredAt = occurredAt.Add(time.Second)
	events = append(events, approved)
	started := base
	started.Type = execution.EventStarted
	started.OccurredAt = occurredAt.Add(2 * time.Second)
	events = append(events, started)
	completed := base
	completed.Type = execution.EventCompleted
	completed.ResultSHA256 = execution.HashText(result)
	completed.OccurredAt = occurredAt.Add(3 * time.Second)
	events = append(events, completed)

	for _, event := range events {
		if _, inserted, err := store.AppendExecutionEvent(context.Background(), event); err != nil {
			t.Fatalf("append %s execution event: %v", event.Type, err)
		} else if !inserted {
			t.Fatalf("execution event %s replayed unexpectedly", event.Type)
		}
	}
}

// TestAutoCheckpointSegmentBoundaryProjectsCompletedEffect is the regression
// for the deterministic long-AUTO brick: a logical turn that performs one
// non-read-only effect and then reaches the iteration watchdog must continue
// into segment 2. Before the fix the continuation launched at snapshot cursor
// 0, so the agent's strict pre-provider scan flagged the already-answered
// effect as "newer than the session snapshot" and the segment died in
// milliseconds without any provider work.
func TestAutoCheckpointSegmentBoundaryProjectsCompletedEffect(t *testing.T) {
	fixture := newAutoCheckpointProjectionFixture(t)
	fixture.armAutoSegment(time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC))

	updated, command := fixture.m.Update(AgentDoneMsg{
		TurnID: "turn-root", SegmentTurnID: "turn-root",
		Err: &agent.AutoIterationCheckpointError{
			ProgressDigest: "progress-a", Iterations: 40,
			ToolCalls: 1, SuccessfulToolCalls: 1, DistinctSuccessfulCalls: 1,
			EffectfulSuccessfulCalls: 1,
		},
	})
	m := updated.(*Model)
	if command == nil {
		t.Fatalf("checkpoint did not schedule segment 2: state=%v entries=%#v", m.state, m.entries)
	}
	if m.state != StateWaiting {
		t.Fatalf("checkpoint state = %v, want StateWaiting", m.state)
	}
	done := awaitCommandMessage[AgentDoneMsg](t, commandMessages(command), 15*time.Second)
	var unresolved *agent.UnresolvedExecutionError
	if errors.As(done.Err, &unresolved) {
		t.Fatalf("segment 2 died in the strict pre-provider scan at cursor %d: %v",
			unresolved.SnapshotCursor, done.Err)
	}
	if done.Err != nil {
		t.Fatalf("segment 2 failed: %v", done.Err)
	}
	// The segment boundary is a projection boundary: the durable cursor must
	// already be past the answered effect before segment 2 is dispatched.
	if m.executionCursor <= 0 {
		t.Fatalf("segment boundary left the snapshot cursor at %d", m.executionCursor)
	}
	if got := fixture.client.calls.Load(); got == 0 {
		t.Fatal("segment 2 reached no provider work")
	}
	for _, entry := range m.entries {
		if entry.Kind == "error" {
			t.Fatalf("productive checkpoint rendered an error: %q", entry.Content)
		}
	}
}

// TestSettledProjectionWithdrawsItsOwnRepairInstruction covers the second half
// of the same brick: when settlement advances the snapshot cursor past the
// effect a run just flagged, `sonar session repair` would answer "already
// current". The transcript must not keep telling the user to run it, and no
// recovery latch may survive a repair that is a no-op.
func TestSettledProjectionWithdrawsItsOwnRepairInstruction(t *testing.T) {
	fixture := newAutoCheckpointProjectionFixture(t)
	m := fixture.m
	m.state = StateStreaming
	m.now = func() time.Time { return time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC) }

	// Reproduce the exact failure a strict pre-provider scan reports for an
	// answered non-read-only effect that is newer than the saved snapshot.
	unresolved := &agent.UnresolvedExecutionError{
		SessionID: fixture.sessionID, WorkspaceID: fixture.workspaceID,
		SnapshotCursor: 0, TurnID: "turn_auto_segment_one",
		ExecutionID: "exec_auto_segment", ToolName: "write",
		EventType: execution.EventCompleted,
		Cause:     errors.New("completed effect is newer than the session snapshot and must be projected before provider work"),
	}
	// A latch left by an earlier hazard must not outlive this settlement.
	m.standaloneRecovery = &standaloneRecoveryState{target: *unresolved}

	updated, _ := m.Update(AgentDoneMsg{TurnID: "turn-root", SegmentTurnID: "turn-root", Err: unresolved})
	m = updated.(*Model)

	if m.executionCursor <= 0 {
		t.Fatalf("settlement did not advance the snapshot cursor: %d", m.executionCursor)
	}
	if m.standaloneRecovery != nil {
		t.Fatalf("a repaired projection left the session latched: %#v", m.standaloneRecovery)
	}
	for _, entry := range m.entries {
		if entry.Kind == "error" && strings.Contains(entry.Content, "sonar session repair") {
			t.Fatalf("transcript still demands a no-op repair: %q", entry.Content)
		}
	}
	withdrawn := false
	for _, entry := range m.entries {
		if entry.Kind == "system" && strings.Contains(entry.Content, "Recovery resolved") {
			withdrawn = true
		}
	}
	if !withdrawn {
		t.Fatalf("withdrawal receipt missing: %#v", m.entries)
	}
	// The transcript must stay encodable: a withdrawn notice is removed, never
	// rewritten in place, because block lifecycles are monotonic.
	if err := m.persistSessionState(context.Background()); err != nil {
		t.Fatalf("persist after withdrawal: %v", err)
	}
}

// TestUnrepairedProjectionKeepsItsRecoveryNotice is the negative control: when
// settlement cannot advance the cursor, the instruction must survive.
func TestUnrepairedProjectionKeepsItsRecoveryNotice(t *testing.T) {
	fixture := newAutoCheckpointProjectionFixture(t)
	m := fixture.m
	// Drop the tool receipt so the effect can no longer be proven projected.
	m.agent.ReplaceMessages([]llm.Message{{Role: "user", Content: "do the long job"}})
	m.state = StateStreaming
	m.now = func() time.Time { return time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC) }

	unresolved := &agent.UnresolvedExecutionError{
		SessionID: fixture.sessionID, WorkspaceID: fixture.workspaceID,
		SnapshotCursor: 0, TurnID: "turn_auto_segment_one",
		ExecutionID: "exec_auto_segment", ToolName: "write",
		EventType: execution.EventCompleted,
		Cause:     errors.New("completed effect is newer than the session snapshot and must be projected before provider work"),
	}
	updated, _ := m.Update(AgentDoneMsg{TurnID: "turn-root", SegmentTurnID: "turn-root", Err: unresolved})
	m = updated.(*Model)

	if m.executionCursor != 0 {
		t.Fatalf("unprojected effect advanced the cursor to %d", m.executionCursor)
	}
	kept := false
	for _, entry := range m.entries {
		if entry.Kind == "error" && strings.Contains(entry.Content, "sonar session repair") {
			kept = true
		}
		if entry.Kind == "system" && strings.Contains(entry.Content, "Recovery resolved") {
			t.Fatalf("an unrepaired projection was reported as resolved: %q", entry.Content)
		}
	}
	if !kept {
		t.Fatalf("unrepaired projection lost its repair instruction: %#v", m.entries)
	}
}

// TestAutoCheckpointStopsCleanlyWhenProjectionBoundaryFails proves the segment
// boundary fails closed: an effect the transcript cannot account for must stop
// the continuation instead of launching a segment that is guaranteed to die.
func TestAutoCheckpointStopsCleanlyWhenProjectionBoundaryFails(t *testing.T) {
	fixture := newAutoCheckpointProjectionFixture(t)
	// Drop the tool receipt: the durable effect can no longer be proven present
	// in the snapshot the boundary would save.
	fixture.m.agent.ReplaceMessages([]llm.Message{{Role: "user", Content: "do the long job"}})
	fixture.armAutoSegment(time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC))

	updated, command := fixture.m.Update(AgentDoneMsg{
		TurnID: "turn-root", SegmentTurnID: "turn-root",
		Err: &agent.AutoIterationCheckpointError{
			ProgressDigest: "progress-a", Iterations: 40,
			ToolCalls: 1, SuccessfulToolCalls: 1, EffectfulSuccessfulCalls: 1,
		},
	})
	m := updated.(*Model)
	if m.state != StateIdle {
		t.Fatalf("failed projection boundary left state %v, want StateIdle", m.state)
	}
	if m.executionCursor != 0 {
		t.Fatalf("failed projection boundary advanced the cursor to %d", m.executionCursor)
	}
	if m.turnSegmentID != "" && m.turnSegmentID != "turn-root" {
		t.Fatalf("failed projection boundary minted continuation identity %q", m.turnSegmentID)
	}
	if got := fixture.client.calls.Load(); got != 0 {
		t.Fatalf("failed projection boundary still dispatched %d provider calls", got)
	}
	if command != nil {
		// Any batched presentation clock may still tick; a second segment may not.
		messages := commandMessages(command)
		settle := time.NewTimer(250 * time.Millisecond)
		defer settle.Stop()
		for done := false; !done; {
			select {
			case message := <-messages:
				if segment, ok := message.(AgentDoneMsg); ok {
					t.Fatalf("failed projection boundary launched a segment: %#v", segment)
				}
			case <-settle.C:
				done = true
			}
		}
	}
	stopped := false
	for _, entry := range m.entries {
		if entry.Kind == "error" && strings.Contains(entry.Content, "AUTO stopped safely at a continuation checkpoint") {
			stopped = true
		}
	}
	if !stopped {
		t.Fatalf("failed projection boundary left no safe-stop receipt: %#v", m.entries)
	}
}
