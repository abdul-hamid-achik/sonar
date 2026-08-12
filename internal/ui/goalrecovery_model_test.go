package ui

import (
	"context"
	"errors"
	"image/color"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/abdul-hamid-achik/sonar/internal/controlplane"
	"github.com/abdul-hamid-achik/sonar/internal/db"
	"github.com/abdul-hamid-achik/sonar/internal/execution"
	"github.com/abdul-hamid-achik/sonar/internal/goal"
	"github.com/abdul-hamid-achik/sonar/internal/reconciliation"
)

type goalRecoveryCoordinatorFixture struct {
	m           *Model
	store       *db.Store
	lease       *db.ExecutionSessionLease
	client      *goalCountingClient
	workspaceID string
	sessionID   int64
	turnID      string
	observedAt  time.Time
}

func newGoalRecoveryCoordinatorFixture(t *testing.T, width, height, members int) *goalRecoveryCoordinatorFixture {
	t.Helper()
	workspaceID, err := canonicalWorkspaceID(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store, err := db.OpenPath(filepath.Join(t.TempDir(), "recovery.db"))
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.CreateSession(context.Background(), db.CreateSessionParams{
		Title: "coordinator recovery", Model: "goal-test-model", Mode: "AUTO", WorkspaceID: workspaceID,
	})
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	const turnID = "turn_recovery_ui"
	runtime := newUIGoalRuntime(t, session.ID, goal.BudgetLimits{MaxContinuationTurns: 4, MaxEvalTokens: 1000})
	recordUIGoalTurn(t, runtime, goal.AdmissionInitial, goal.TurnReport{
		TurnID: turnID, EvalTokens: 9, Summary: "provider receipt was lost",
		OutcomeUnknown: true, OutcomeRef: "receipt_recovery_ui",
	})

	baseTime := time.Date(2026, time.July, 12, 18, 30, 0, 0, time.UTC)
	for index := 0; index < members; index++ {
		appendGoalRecoveryStartedExecution(t, store, session.ID, workspaceID, turnID, index, baseTime)
	}

	client := &goalCountingClient{}
	m := newGoalRuntimeTestModel(t, client)
	m.agent.SetWorkDir(workspaceID)
	m.agent.SetExecutionLedger(store)
	m.agent.SetExecutionSessionID(session.ID, "")
	m.agent.SetExecutionSnapshotCursor(0)
	m.SetSessionStore(store)
	m.sessionID = session.ID
	m.executionCursor = 0
	m.goalRuntime = runtime
	m.mode = ModeAuto
	m.now = func() time.Time { return baseTime.Add(15 * time.Minute) }
	raw, err := encodeSessionState(m)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if err := store.SaveSessionState(context.Background(), session.ID, raw); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	record, err := store.GetSessionStateRecord(context.Background(), session.ID)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	lease, err := store.AcquireExecutionSessionLease(context.Background(), session.ID, workspaceID)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	m.executionLease = lease
	if err := m.initializeSessionStateRevision(record.Revision); err != nil {
		_ = lease.Close()
		_ = store.Close()
		t.Fatal(err)
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: width, Height: height})
	m = updated.(*Model)

	fixture := &goalRecoveryCoordinatorFixture{
		m: m, store: store, lease: lease, client: client,
		workspaceID: workspaceID, sessionID: session.ID, turnID: turnID,
		observedAt: baseTime.Add(15 * time.Minute),
	}
	t.Cleanup(func() {
		_ = lease.Close()
		_ = store.Close()
	})
	return fixture
}

func appendGoalRecoveryStartedExecution(t *testing.T, store *db.Store, sessionID int64, workspaceID, turnID string, index int, occurredAt time.Time) {
	t.Helper()
	suffix := string(rune('a' + index))
	base := execution.Event{
		Identity: execution.Identity{
			SessionID: sessionID, WorkspaceID: workspaceID,
			RunID: "run_recovery_" + suffix, TurnID: turnID,
			ExecutionID: "exec_recovery_" + suffix, IdempotencyKey: "idem_recovery_" + suffix,
			ProviderCallID: "provider_recovery_" + suffix, CanonicalCallID: "call_recovery_" + suffix,
			ToolName: "secret_backend_tool_" + suffix, Iteration: 1, Ordinal: index + 1,
			Kind: execution.KindBuiltin, EffectClass: execution.Effectful,
		},
		Type: execution.EventRequested, Approval: execution.ApprovalNotApplicable,
		ArgumentsSHA256: execution.HashText("private arguments " + suffix), OccurredAt: occurredAt.Add(time.Duration(index) * time.Minute),
	}
	for _, event := range []execution.Event{
		base,
		func() execution.Event {
			event := base
			event.Type = execution.EventApproved
			event.Approval = execution.ApprovalEmbedding
			event.OccurredAt = event.OccurredAt.Add(time.Second)
			return event
		}(),
		func() execution.Event {
			event := base
			event.Type = execution.EventStarted
			event.OccurredAt = event.OccurredAt.Add(2 * time.Second)
			return event
		}(),
	} {
		if _, inserted, err := store.AppendExecutionEvent(context.Background(), event); err != nil {
			t.Fatal(err)
		} else if !inserted {
			t.Fatalf("execution event %s replayed unexpectedly", event.Type)
		}
	}
}

func loadGoalRecoveryProjection(t *testing.T, fixture *goalRecoveryCoordinatorFixture, inspector bool) {
	t.Helper()
	var command tea.Cmd
	if inspector {
		command = fixture.m.showGoal()
	} else {
		command = fixture.m.ensureCurrentGoalRecoveryProjection(false)
	}
	if command == nil || !fixture.m.goalRecoveryLoadRunning {
		t.Fatal("reconciliation group load was not scheduled")
	}
	result := awaitCommandMessage[goalRecoveryLoadResultMsg](t, commandMessages(command), 3*time.Second)
	updated, childCommand := fixture.m.Update(result)
	fixture.m = updated.(*Model)
	if childCommand != nil {
		_ = commandMessages(childCommand)
	}
	if result.err != nil || fixture.m.goalRecoveryProjection.errorText != "" {
		t.Fatalf("load recovery projection: result=%v projection=%q", result.err, fixture.m.goalRecoveryProjection.errorText)
	}
}

func goalRecoveryItemOfKind(t *testing.T, items []GoalRecoveryItem, kind GoalRecoveryItemKind, actionable bool) GoalRecoveryItem {
	t.Helper()
	for _, item := range items {
		if item.Kind == kind && item.Actionable == actionable {
			return item
		}
	}
	t.Fatalf("no recovery item kind=%s actionable=%v in %#v", kind, actionable, items)
	return GoalRecoveryItem{}
}

func goalRecoveryExecutionDraft(observation GoalRecoveryObservation) GoalRecoveryDraft {
	return GoalRecoveryDraft{
		Observation: observation, Source: GoalRecoveryVerificationCheck,
		Summary: "inspected the external system and verified the exact effect", Reference: "check:sha256:abc123",
	}
}

func goalRecoveryTurnDraft() GoalRecoveryDraft {
	return GoalRecoveryDraft{
		Observation: GoalRecoveryTurnAbandonedAfterInspection, Source: GoalRecoveryOperatorObservation,
		Summary: "inspected every execution receipt and abandoned only the lost provider turn", Reference: "operator-log:turn-recovery-ui",
	}
}

func inspectorRecoveryAction(t *testing.T, inspector *GoalInspector) (int, bool, string) {
	t.Helper()
	if inspector == nil {
		t.Fatal("goal inspector is nil")
		return 0, false, ""
	}
	for index, action := range inspector.actions {
		if action.Spec.ID == goalInspectorRecoveryActionID {
			return index, action.Enabled, action.DisabledReason
		}
	}
	t.Fatal("goal inspector has no Recovery action")
	return 0, false, ""
}

func TestGoalRecoveryZeroToolGroupLoadsWithoutOpeningInspectorAndRetainsCompactEscape(t *testing.T) {
	fixture := newGoalRecoveryCoordinatorFixture(t, 30, 12, 0)
	loadGoalRecoveryProjection(t, fixture, false)
	projection := fixture.m.goalRecoveryProjection
	if projection.executionMemberCount != 0 || projection.remainingExecutions != 0 || len(projection.items) != 1 {
		t.Fatalf("zero-tool projection = %#v", projection)
	}
	parent := goalRecoveryItemOfKind(t, projection.items, GoalRecoveryTurnBoundary, true)
	if parent.ItemID != projection.groupItemID || parent.TurnID != fixture.turnID {
		t.Fatalf("zero-tool parent = %#v projection=%#v", parent, projection)
	}
	if fixture.m.overlay != OverlayNone {
		t.Fatalf("background ensure opened overlay %d", fixture.m.overlay)
	}
	group, err := fixture.store.GetReconciliationGroup(context.Background(), fixture.sessionID, fixture.workspaceID, projection.groupItemID)
	if err != nil || group.ExecutionMemberCount != 0 {
		t.Fatalf("durable zero-tool group = %#v err=%v", group, err)
	}

	if command := fixture.m.showGoal(); command != nil {
		t.Fatal("exact loaded projection scheduled a duplicate ensure")
	}
	_, enabled, reason := inspectorRecoveryAction(t, fixture.m.goalInspectorState)
	if !enabled || reason != "" {
		t.Fatalf("zero-tool Recovery enabled=%v reason=%q", enabled, reason)
	}
	updated, _ := fixture.m.Update(enterKey())
	fixture.m = updated.(*Model)
	if fixture.m.overlay != OverlayGoalRecovery || fixture.m.goalRecoveryState == nil {
		t.Fatal("parent did not route the Inspector Recovery action to its child modal")
	}
	view := fixture.m.View().Content
	plain := ansi.Strip(view)
	if !strings.Contains(plain, "Turn effects unknown") || strings.Contains(strings.ToLower(plain), "tool may have run") {
		t.Fatalf("zero-tool Recovery used tool-specific warning:\n%s", plain)
	}
	assertRenderedLinesFit(t, view, 30)
	assertRenderedHeightFits(t, view, 12)
	updated, _ = fixture.m.Update(escKey())
	fixture.m = updated.(*Model)
	if fixture.m.overlay != OverlayGoalInspector || fixture.m.goalRecoveryLoadRunning {
		t.Fatalf("compact Escape overlay=%d loadRunning=%v", fixture.m.overlay, fixture.m.goalRecoveryLoadRunning)
	}
	if fixture.client.calls.Load() != 0 {
		t.Fatalf("zero-tool inspection called provider %d time(s)", fixture.client.calls.Load())
	}
}

func TestGoalRecoveryCloseNeverArmsDiscardedLoadCommand(t *testing.T) {
	fixture := newGoalRecoveryCoordinatorFixture(t, 80, 24, 0)
	loadGoalRecoveryProjection(t, fixture, true)
	fixture.m.openGoalRecovery()
	fixture.m.goalRecoveryProjection.errorText = "retryable load error"
	fixture.m.closeGoalRecovery()
	if fixture.m.goalRecoveryLoadRunning {
		t.Fatal("closing nested Recovery armed a load whose command was discarded")
	}
	if fixture.m.overlay != OverlayGoalInspector {
		t.Fatalf("close overlay = %d", fixture.m.overlay)
	}
}

func TestGoalRecoveryMultiMemberPartialReceiptsRefreshThenFinalReceiptHydratesTogether(t *testing.T) {
	fixture := newGoalRecoveryCoordinatorFixture(t, 80, 24, 2)
	loadGoalRecoveryProjection(t, fixture, true)
	projection := fixture.m.goalRecoveryProjection
	if projection.executionMemberCount != 2 || projection.remainingExecutions != 2 || len(projection.items) != 3 ||
		goalRecoveryActionableItems(projection.items) != 2 {
		t.Fatalf("initial multi-member projection = %#v", projection)
	}
	for _, item := range projection.items {
		if item.Tool != "" || strings.Contains(item.Summary, "secret_backend_tool") || strings.Contains(item.Summary, "private arguments") {
			t.Fatalf("unsanitized recovery item = %#v", item)
		}
	}
	parent := goalRecoveryItemOfKind(t, projection.items, GoalRecoveryTurnBoundary, false)
	if !strings.Contains(parent.DisabledReason, "2 execution members") {
		t.Fatalf("initial parent gate = %q", parent.DisabledReason)
	}
	inspectorDocument := ansi.Strip(fixture.m.goalInspectorState.buildDocument())
	if !strings.Contains(inspectorDocument, "retry remains blocked") || strings.Contains(inspectorDocument, "AUTO stays paused") {
		t.Fatalf("Inspector recovery status disagreed with blocked goal:\n%s", inspectorDocument)
	}
	fixture.m.openGoalRecovery()
	if fixture.m.goalRecoveryState == nil {
		t.Fatal("multi-member Recovery did not open")
	}

	initialGoal := snapshotUIGoal(t, fixture.m.goalRuntime)
	initialRevision := fixture.m.sessionStateRevision
	initialCursor := fixture.m.executionCursor
	for memberIndex := 0; memberIndex < 2; memberIndex++ {
		member := goalRecoveryItemOfKind(t, fixture.m.goalRecoveryProjection.items, GoalRecoveryExecutionEffect, true)
		event := GoalRecoveryEvent{Action: GoalRecoveryActionApply, ItemID: member.ItemID, Draft: goalRecoveryExecutionDraft(GoalRecoveryEffectNotApplied)}
		applyCommand := fixture.m.beginGoalRecoveryApply(event)
		if applyCommand == nil || !fixture.m.goalRecoveryApplyRunning {
			t.Fatalf("member %d Apply did not start", memberIndex)
		}
		if duplicate := fixture.m.beginGoalRecoveryApply(event); duplicate != nil {
			t.Fatalf("member %d admitted duplicate Apply", memberIndex)
		}
		if plain := ansi.Strip(fixture.m.goalRecoveryState.View()); !strings.Contains(plain, "Recording immutable evidence") {
			t.Fatalf("member %d missing in-flight presentation:\n%s", memberIndex, plain)
		}
		applyResult := awaitCommandMessage[goalRecoveryApplyResultMsg](t, commandMessages(applyCommand), 3*time.Second)
		if applyResult.err != nil {
			t.Fatalf("member %d apply: %v", memberIndex, applyResult.err)
		}
		updated, refreshCommand := fixture.m.Update(applyResult)
		fixture.m = updated.(*Model)
		if refreshCommand == nil || !fixture.m.goalRecoveryLoadRunning {
			t.Fatalf("member %d did not schedule projection refresh", memberIndex)
		}
		partialGoal := snapshotUIGoal(t, fixture.m.goalRuntime)
		if partialGoal.State != goal.StateBlocked || partialGoal.Blocker == nil || partialGoal.ID != initialGoal.ID ||
			fixture.m.sessionStateRevision != initialRevision || fixture.m.executionCursor != initialCursor {
			t.Fatalf("partial member changed goal/cursor/revision: goal=%#v cursor=%d revision=%d", partialGoal, fixture.m.executionCursor, fixture.m.sessionStateRevision)
		}

		refreshResult := awaitCommandMessage[goalRecoveryLoadResultMsg](t, commandMessages(refreshCommand), 3*time.Second)
		updated, _ = fixture.m.Update(refreshResult)
		fixture.m = updated.(*Model)
		wantRemaining := 1 - memberIndex
		if fixture.m.goalRecoveryProjection.remainingExecutions != wantRemaining {
			t.Fatalf("member %d remaining=%d want=%d", memberIndex, fixture.m.goalRecoveryProjection.remainingExecutions, wantRemaining)
		}
	}
	parent = goalRecoveryItemOfKind(t, fixture.m.goalRecoveryProjection.items, GoalRecoveryTurnBoundary, true)
	if fixture.m.sessionStateRevision != initialRevision {
		t.Fatalf("all member receipts advanced revision to %d", fixture.m.sessionStateRevision)
	}

	states, err := fixture.store.ListControlStates(context.Background(), controlplane.Query{
		SessionID: fixture.sessionID, WorkspaceID: fixture.workspaceID,
		Kind: controlplane.KindExecutionReconciliation, Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range states {
		if state.Resolution == nil {
			t.Fatalf("member resolution missing: %#v", state)
		}
		envelope, err := reconciliation.Parse(state.Resolution.EvidenceJSON, state.Resolution.EvidenceSHA256)
		if err != nil {
			t.Fatal(err)
		}
		if !envelope.Source.ObservedAt.Equal(fixture.observedAt) || envelope.Source.ObservedAt.Location() != time.UTC {
			t.Fatalf("member observed_at = %v (%v), want one UTC instant %v", envelope.Source.ObservedAt, envelope.Source.ObservedAt.Location(), fixture.observedAt)
		}
	}

	parentCommand := fixture.m.beginGoalRecoveryApply(GoalRecoveryEvent{
		Action: GoalRecoveryActionApply, ItemID: parent.ItemID, Draft: goalRecoveryTurnDraft(),
	})
	if parentCommand == nil {
		t.Fatal("final turn parent Apply did not start")
		return
	}
	parentResult := awaitCommandMessage[goalRecoveryApplyResultMsg](t, commandMessages(parentCommand), 3*time.Second)
	if parentResult.err != nil || !parentResult.receipt.GoalCleared {
		t.Fatalf("parent result = %#v err=%v", parentResult.receipt, parentResult.err)
	}
	updated, resultCommand := fixture.m.Update(parentResult)
	fixture.m = updated.(*Model)
	if resultCommand != nil {
		t.Fatal("final parent hydration scheduled provider/Cortex/resume work")
	}
	finalGoal := snapshotUIGoal(t, fixture.m.goalRuntime)
	if finalGoal.State != goal.StatePaused && finalGoal.State != goal.StateExhausted {
		t.Fatalf("final goal state = %s", finalGoal.State)
	}
	if finalGoal.Blocker != nil || fixture.m.executionCursor != parentResult.receipt.ExecutionCursor ||
		fixture.m.sessionStateRevision != parentResult.receipt.SessionRevision ||
		fixture.m.sessionStateRevision != initialRevision+1 {
		t.Fatalf("final hydration goal=%#v cursor=%d revision=%d receipt=%#v", finalGoal, fixture.m.executionCursor, fixture.m.sessionStateRevision, parentResult.receipt)
	}
	if fixture.m.overlay != OverlayGoalInspector || fixture.m.goalInspectorState == nil ||
		fixture.m.goalRecoveryState != nil || fixture.m.goalRecoveryProjection.goalID != "" {
		t.Fatalf("final presentation overlay=%d inspector=%v recovery=%v projection=%#v", fixture.m.overlay, fixture.m.goalInspectorState != nil, fixture.m.goalRecoveryState != nil, fixture.m.goalRecoveryProjection)
	}
	if fixture.client.calls.Load() != 0 {
		t.Fatalf("reconciliation called provider %d time(s)", fixture.client.calls.Load())
	}
	record, err := fixture.store.GetSessionStateRecord(context.Background(), fixture.sessionID)
	if err != nil || record.Revision != fixture.m.sessionStateRevision {
		t.Fatalf("durable final revision=%d model=%d err=%v", record.Revision, fixture.m.sessionStateRevision, err)
	}
	persisted, err := decodeSessionState(record.StateJSON)
	if err != nil || persisted.Goal == nil || persisted.Goal.State != finalGoal.State || persisted.ExecutionCursor != fixture.m.executionCursor {
		t.Fatalf("durable final state=%#v err=%v", persisted, err)
	}
}

func TestGoalRecoveryRejectsStaleLoadAndApplyTokens(t *testing.T) {
	fixture := newGoalRecoveryCoordinatorFixture(t, 80, 24, 1)
	loadGoalRecoveryProjection(t, fixture, true)
	originalGroup := fixture.m.goalRecoveryProjection.groupItemID
	scope, err := fixture.m.goalRecoveryAuthority(snapshotUIGoal(t, fixture.m.goalRuntime))
	if err != nil {
		t.Fatal(err)
	}
	fixture.m.goalRecoveryLoadToken++
	fixture.m.goalRecoveryLoadRunning = true
	fixture.m.goalRecoveryLoadScope = scope
	updated, _ := fixture.m.Update(goalRecoveryLoadResultMsg{
		token: fixture.m.goalRecoveryLoadToken - 1, scope: scope,
		projection: goalRecoveryProjection{groupItemID: "stale_group"},
	})
	fixture.m = updated.(*Model)
	if fixture.m.goalRecoveryProjection.groupItemID != originalGroup || !fixture.m.goalRecoveryLoadRunning {
		t.Fatalf("stale load changed projection/running: %#v", fixture.m.goalRecoveryProjection)
	}
	fixture.m.goalRecoveryLoadRunning = false
	fixture.m.goalRecoveryLoadScope = goalRecoveryOperationScope{}

	fixture.m.openGoalRecovery()
	member := goalRecoveryItemOfKind(t, fixture.m.goalRecoveryProjection.items, GoalRecoveryExecutionEffect, true)
	command := fixture.m.beginGoalRecoveryApply(GoalRecoveryEvent{
		Action: GoalRecoveryActionApply, ItemID: member.ItemID, Draft: goalRecoveryExecutionDraft(GoalRecoveryEffectApplied),
	})
	if command == nil {
		t.Fatal("Apply did not start")
		return
	}
	before := snapshotUIGoal(t, fixture.m.goalRuntime)
	updated, _ = fixture.m.Update(goalRecoveryApplyResultMsg{
		token: fixture.m.goalRecoveryApplyToken + 1, scope: scope, itemID: member.ItemID, kind: member.Kind,
	})
	fixture.m = updated.(*Model)
	after := snapshotUIGoal(t, fixture.m.goalRuntime)
	if !fixture.m.goalRecoveryApplyRunning || after.State != before.State || after.Blocker == nil || fixture.client.calls.Load() != 0 {
		t.Fatalf("stale apply token changed authority: before=%#v after=%#v running=%v", before, after, fixture.m.goalRecoveryApplyRunning)
	}
	fixture.m.resetGoalRecoveryPresentation()
}

func TestGoalRecoveryCorrelatedStaleLoadCannotLeaveInspectorLoading(t *testing.T) {
	fixture := newGoalRecoveryCoordinatorFixture(t, 80, 24, 0)
	loadCommand := fixture.m.showGoal()
	if loadCommand == nil || !fixture.m.goalRecoveryLoadRunning {
		t.Fatal("Inspector did not start recovery load")
	}
	before := ansi.Strip(fixture.m.goalInspectorState.buildDocument())
	if !strings.Contains(before, "loading durable recovery group") {
		t.Fatalf("Inspector did not present loading state:\n%s", before)
	}

	token := fixture.m.goalRecoveryLoadToken
	scope := fixture.m.goalRecoveryLoadScope
	if err := fixture.m.initializeSessionStateRevision(scope.revision + 1); err != nil {
		t.Fatal(err)
	}
	updated, _ := fixture.m.Update(goalRecoveryLoadResultMsg{token: token, scope: scope})
	fixture.m = updated.(*Model)

	if fixture.m.goalRecoveryLoadRunning {
		t.Fatal("correlated stale result left load marked in flight")
	}
	projection := fixture.m.goalRecoveryProjection
	if projection.sessionID != scope.sessionID || projection.goalID != scope.goalID ||
		projection.revision != scope.revision || projection.errorText == "" {
		t.Fatalf("stale load error was not scoped: %#v", projection)
	}
	after := ansi.Strip(fixture.m.goalInspectorState.buildDocument())
	if strings.Contains(after, "loading durable recovery group") {
		t.Fatalf("Inspector remained stuck on loading after stale result:\n%s", after)
	}
}

func TestGoalRecoveryLeaseFailureStaysInlineAndBlocked(t *testing.T) {
	fixture := newGoalRecoveryCoordinatorFixture(t, 80, 24, 1)
	loadGoalRecoveryProjection(t, fixture, true)
	fixture.m.openGoalRecovery()
	member := goalRecoveryItemOfKind(t, fixture.m.goalRecoveryProjection.items, GoalRecoveryExecutionEffect, true)
	beforeRevision := fixture.m.sessionStateRevision
	command := fixture.m.beginGoalRecoveryApply(GoalRecoveryEvent{
		Action: GoalRecoveryActionApply, ItemID: member.ItemID, Draft: goalRecoveryExecutionDraft(GoalRecoveryEffectCompensated),
	})
	if command == nil {
		t.Fatal("Apply did not start")
		return
	}
	if err := fixture.lease.Close(); err != nil {
		t.Fatal(err)
	}
	result := awaitCommandMessage[goalRecoveryApplyResultMsg](t, commandMessages(command), 3*time.Second)
	if !errors.Is(result.err, db.ErrControlLeaseRequired) {
		t.Fatalf("closed lease apply error = %v", result.err)
	}
	updated, _ := fixture.m.Update(result)
	fixture.m = updated.(*Model)
	snapshot := snapshotUIGoal(t, fixture.m.goalRuntime)
	if snapshot.State != goal.StateBlocked || snapshot.Blocker == nil || fixture.m.sessionStateRevision != beforeRevision ||
		fixture.m.goalRecoveryState == nil || !strings.Contains(fixture.m.goalRecoveryState.errorText, "lease") {
		t.Fatalf("lease failure mutated/hidden state: goal=%#v revision=%d error=%q", snapshot, fixture.m.sessionStateRevision, fixture.m.goalRecoveryState.errorText)
	}
	if fixture.client.calls.Load() != 0 {
		t.Fatalf("lease failure called provider %d time(s)", fixture.client.calls.Load())
	}
}

func TestGoalRecoveryThemeResizeMouseAndSessionReset(t *testing.T) {
	fixture := newGoalRecoveryCoordinatorFixture(t, 80, 24, 0)
	loadGoalRecoveryProjection(t, fixture, true)
	fixture.m.openGoalRecovery()
	updated, _ := fixture.m.Update(tea.BackgroundColorMsg{Color: color.White})
	fixture.m = updated.(*Model)
	if fixture.m.goalRecoveryState == nil || fixture.m.goalRecoveryState.isDark {
		t.Fatal("Recovery child did not receive adaptive light theme")
	}
	updated, _ = fixture.m.Update(tea.WindowSizeMsg{Width: 30, Height: 12})
	fixture.m = updated.(*Model)
	if fixture.m.goalRecoveryState.width != 30 || fixture.m.goalRecoveryState.height != 12 || !fixture.m.goalRecoveryState.compact() {
		t.Fatalf("Recovery resize = %dx%d compact=%v", fixture.m.goalRecoveryState.width, fixture.m.goalRecoveryState.height, fixture.m.goalRecoveryState.compact())
	}
	fixture.m.viewport.SetHeight(2)
	fixture.m.setTestTranscriptContent(strings.Repeat("transcript line\n", 40))
	fixture.m.setTranscriptYOffset(10)
	before := fixture.m.transcriptYOffset()
	updated, _ = fixture.m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	fixture.m = updated.(*Model)
	if fixture.m.transcriptYOffset() != before {
		t.Fatalf("hidden transcript moved under Recovery: before=%d after=%d", before, fixture.m.transcriptYOffset())
	}
	fixture.m.resetConversationSession()
	if fixture.m.goalRecoveryState != nil || fixture.m.goalRecoveryProjection.goalID != "" ||
		fixture.m.goalRecoveryLoadRunning || fixture.m.goalRecoveryApplyRunning {
		t.Fatalf("session reset retained Recovery state: projection=%#v", fixture.m.goalRecoveryProjection)
	}
}
