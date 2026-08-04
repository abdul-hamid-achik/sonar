package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/db"
	"github.com/abdul-hamid-achik/sonar/internal/execution"
	"github.com/abdul-hamid-achik/sonar/internal/sessionref"
)

type fakeExecutionRecoveryStore struct {
	inspection  db.StandaloneExecutionReconciliationInspection
	inspections map[string]db.StandaloneExecutionReconciliationInspection
	inspectErr  error
	inspected   int
	acquired    int
	lease       *db.ExecutionSessionLease
	pending     []execution.State
	pendingErr  error
	listed      int
	receipts    map[string]db.StandaloneExecutionReconciliationReceipt
	resolved    []db.ResolveStandaloneExecutionReconciliationRequest
	publicID    string
	sessionID   int64
}

func (s *fakeExecutionRecoveryStore) ResolveSessionRef(_ context.Context, ref string) (db.Session, error) {
	publicID, err := sessionref.Parse(ref)
	if err != nil {
		return db.Session{}, err
	}
	id := s.sessionID
	if id == 0 {
		id = s.inspection.SessionID
	}
	if id == 0 && len(s.pending) > 0 {
		id = s.pending[0].Identity.SessionID
	}
	if id == 0 {
		id = 17
	}
	// Tests use a fixed public handle; accept it or an explicit store override.
	pid := s.publicID
	if pid == "" {
		pid = "aaaaa11"
	}
	if publicID != pid && publicID != "aaaaa11" {
		// Still allow arbitrary valid handles so error-path tests can resolve first.
		return db.Session{ID: id, PublicID: publicID, WorkspaceID: "/workspace/repo"}, nil
	}
	return db.Session{ID: id, PublicID: pid, WorkspaceID: "/workspace/repo"}, nil
}

func (s *fakeExecutionRecoveryStore) SessionHandle(_ context.Context, sessionID int64) (string, error) {
	pid := s.publicID
	if pid == "" {
		pid = "aaaaa11"
	}
	return pid, nil
}

func (s *fakeExecutionRecoveryStore) ListStandaloneExecutionReconciliationPending(context.Context, int64, string, int) ([]execution.State, error) {
	s.listed++
	return s.pending, s.pendingErr
}

func (s *fakeExecutionRecoveryStore) InspectStandaloneExecutionReconciliation(_ context.Context, _ int64, _ string, executionID string) (db.StandaloneExecutionReconciliationInspection, error) {
	s.inspected++
	if inspection, ok := s.inspections[executionID]; ok {
		return inspection, s.inspectErr
	}
	return s.inspection, s.inspectErr
}

func (s *fakeExecutionRecoveryStore) AcquireExecutionSessionLease(context.Context, int64, string) (*db.ExecutionSessionLease, error) {
	s.acquired++
	if s.lease == nil {
		return nil, errors.New("not expected")
	}
	return s.lease, nil
}

func (s *fakeExecutionRecoveryStore) ResolveStandaloneExecutionReconciliation(_ context.Context, _ *db.ExecutionSessionLease, request db.ResolveStandaloneExecutionReconciliationRequest) (db.StandaloneExecutionReconciliationReceipt, error) {
	s.resolved = append(s.resolved, request)
	if receipt, ok := s.receipts[request.ExecutionID]; ok {
		return receipt, nil
	}
	return db.StandaloneExecutionReconciliationReceipt{}, errors.New("not expected")
}

func TestExecutionRecoverInspectionIsReadOnlyAndPrintsExactApplyTokens(t *testing.T) {
	store := &fakeExecutionRecoveryStore{inspection: db.StandaloneExecutionReconciliationInspection{
		SessionID: 17, WorkspaceID: "/workspace/repo", SessionRevision: 4,
		ExecutionID: "exec_timeout", TurnID: "turn_timeout", ToolName: "bash",
		EventID: 29, EventType: execution.EventOutcomeUnknown,
		EffectClass: execution.EffectUnknown, ArgumentsSHA256: strings.Repeat("a", 64),
		ItemID: "ctrl_execution_timeout",
	}}
	var stdout, stderr bytes.Buffer
	code := handleExecutionRecover(store, "/workspace/repo", []string{"aaaaa11", "exec_timeout"}, &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 || store.inspected != 1 || store.acquired != 0 {
		t.Fatalf("code=%d inspected=%d acquired=%d stdout=%q stderr=%q", code, store.inspected, store.acquired, stdout.String(), stderr.String())
	}
	for _, want := range []string{
		"Inspection is read-only", "--revision 4", "--event-id 29",
		"aaaaa11 @ revision 4", "aaaaa11 exec_timeout", "effect_not_applied", "verification_check",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("inspection missing %q:\n%s", want, stdout.String())
		}
	}

	stdout.Reset()
	stderr.Reset()
	code = handleExecutionRecover(store, "/workspace/repo", []string{"--json", "aaaaa11", "exec_timeout"}, &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("JSON inspection code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var view executionRecoveryView
	if err := json.Unmarshal(stdout.Bytes(), &view); err != nil {
		t.Fatalf("decode JSON inspection: %v (%s)", err, stdout.String())
	}
	if view.SessionID != 17 || !strings.Contains(view.ApplyTemplate, " 17 exec_timeout") || strings.Contains(view.ApplyTemplate, "aaaaa11") {
		t.Fatalf("JSON inspection changed the numeric contract: %#v", view)
	}
}

func TestExecutionRecoverAllListsPendingReadOnly(t *testing.T) {
	store := &fakeExecutionRecoveryStore{pending: []execution.State{
		{
			Identity: execution.Identity{
				SessionID: 17, WorkspaceID: "/workspace/repo", ExecutionID: "exec_one",
				ToolName: "bash", TurnID: "turn_one", EffectClass: execution.EffectUnknown,
			},
			Latest: execution.Event{ID: 29, Type: execution.EventOutcomeUnknown},
		},
		{
			Identity: execution.Identity{
				SessionID: 17, WorkspaceID: "/workspace/repo", ExecutionID: "exec_two",
				ToolName: "mcphub__cortex__cortex_open_task", TurnID: "turn_two", EffectClass: execution.Effectful,
			},
			Latest: execution.Event{ID: 41, Type: execution.EventOutcomeUnknown},
		},
	}}
	var stdout, stderr bytes.Buffer
	code := handleExecutionRecover(store, "/workspace/repo", []string{"aaaaa11", "--all"}, &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	if store.listed != 1 || store.acquired != 0 {
		t.Fatalf("listing acquired a lease or skipped the query: %#v", store)
	}
	out := stdout.String()
	for _, want := range []string{"exec_one", "exec_two", "2 execution(s) pending reconciliation", "Reviewed-set digest", "execution recover aaaaa11 --all --apply --set-digest"} {
		if !strings.Contains(out, want) {
			t.Fatalf("listing missing %q: %s", want, out)
		}
	}

	empty := &fakeExecutionRecoveryStore{}
	stdout.Reset()
	stderr.Reset()
	if code := handleExecutionRecover(empty, "/workspace/repo", []string{"aaaaa11", "--all"}, &stdout, &stderr); code != 0 {
		t.Fatalf("empty listing exit=%d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "No executions are pending reconciliation in session aaaaa11") {
		t.Fatalf("empty listing output = %q", stdout.String())
	}
}

func TestExecutionRecoverAllListingFailsClosedOnOverflow(t *testing.T) {
	store := &fakeExecutionRecoveryStore{pendingErr: fmt.Errorf("%w: more than 100 effective hazards", db.ErrExecutionHazardOverflow)}
	var stdout, stderr bytes.Buffer
	code := handleExecutionRecover(store, "/workspace/repo", []string{"aaaaa11", "--all"}, &stdout, &stderr)
	if code != 1 || stdout.Len() != 0 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{"--all aborted", "exceeds the safe limit of 100", "recover executions individually"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("overflow error missing %q: %s", want, stderr.String())
		}
	}
	if store.listed != 1 || store.acquired != 0 || store.inspected != 0 || len(store.resolved) != 0 {
		t.Fatalf("overflow listing touched unexpected recovery operations: %#v", store)
	}
}

func TestExecutionRecoverAllApplyProcessesEntireBoundedBacklog(t *testing.T) {
	states := []execution.State{
		{Identity: execution.Identity{ExecutionID: "exec_one"}, Latest: execution.Event{ID: 29, Type: execution.EventOutcomeUnknown}},
		{Identity: execution.Identity{ExecutionID: "exec_two"}, Latest: execution.Event{ID: 41, Type: execution.EventOutcomeUnknown}},
	}
	store := &fakeExecutionRecoveryStore{
		lease:   &db.ExecutionSessionLease{},
		pending: states,
		inspections: map[string]db.StandaloneExecutionReconciliationInspection{
			"exec_one": {SessionID: 17, SessionRevision: 4, ExecutionID: "exec_one", EventID: 29},
			"exec_two": {SessionID: 17, SessionRevision: 4, ExecutionID: "exec_two", EventID: 41},
		},
		receipts: map[string]db.StandaloneExecutionReconciliationReceipt{
			"exec_one": {SessionID: 17, SessionRevision: 4, ExecutionID: "exec_one", EventID: 29, ResolutionID: "resolution_one", Inserted: true},
			"exec_two": {SessionID: 17, SessionRevision: 4, ExecutionID: "exec_two", EventID: 41, ResolutionID: "resolution_two", Inserted: true},
		},
	}
	var stdout, stderr bytes.Buffer
	setDigest := executionRecoverySetDigest(17, "/workspace/repo", states)
	code := handleExecutionRecover(store, "/workspace/repo", []string{
		"aaaaa11", "--all", "--apply", "--set-digest", setDigest, "--observation", "effect_not_applied",
		"--source", "verification_check", "--reference", "ref", "--summary", "sum",
		"--observed-at", "2026-07-14T10:00:00Z",
	}, &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{"Reconciled execution exec_one", "Reconciled execution exec_two", "Reconciled 2 execution(s)."} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("batch output missing %q: %s", want, stdout.String())
		}
	}
	if store.acquired != 1 || store.listed != 1 || store.inspected != 2 || len(store.resolved) != 2 {
		t.Fatalf("bounded batch did not process every listed execution: %#v", store)
	}
}

func TestExecutionRecoverAllApplyFailsClosedBeforeMutationOnOverflow(t *testing.T) {
	store := &fakeExecutionRecoveryStore{
		lease:      &db.ExecutionSessionLease{},
		pendingErr: fmt.Errorf("%w: more than 100 effective hazards", db.ErrExecutionHazardOverflow),
	}
	var stdout, stderr bytes.Buffer
	code := handleExecutionRecover(store, "/workspace/repo", []string{
		"aaaaa11", "--all", "--apply", "--set-digest", strings.Repeat("a", 64), "--observation", "effect_not_applied",
		"--source", "verification_check", "--reference", "ref", "--summary", "sum",
		"--observed-at", "2026-07-14T10:00:00Z",
	}, &stdout, &stderr)
	if code != 1 || stdout.Len() != 0 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{"--all aborted", "exceeds the safe limit of 100", "No evidence was recorded"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("overflow error missing %q: %s", want, stderr.String())
		}
	}
	if store.acquired != 1 || store.listed != 1 || store.inspected != 0 || len(store.resolved) != 0 {
		t.Fatalf("overflow apply performed mutation work: %#v", store)
	}
}

func TestExecutionRecoverAllApplyRejectsChangedPendingSetBeforeEvidence(t *testing.T) {
	reviewed := []execution.State{{
		Identity: execution.Identity{ExecutionID: "exec_reviewed"},
		Latest:   execution.Event{ID: 29, Type: execution.EventOutcomeUnknown},
	}}
	current := []execution.State{{
		Identity: execution.Identity{ExecutionID: "exec_new"},
		Latest:   execution.Event{ID: 41, Type: execution.EventOutcomeUnknown},
	}}
	store := &fakeExecutionRecoveryStore{lease: &db.ExecutionSessionLease{}, pending: current}
	var stdout, stderr bytes.Buffer
	code := handleExecutionRecover(store, "/workspace/repo", []string{
		"aaaaa11", "--all", "--apply", "--set-digest", executionRecoverySetDigest(17, "/workspace/repo", reviewed),
		"--observation", "effect_not_applied", "--source", "verification_check",
		"--reference", "ref", "--summary", "sum", "--observed-at", "2026-07-14T10:00:00Z",
	}, &stdout, &stderr)
	if code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "pending reconciliation set changed") ||
		!strings.Contains(stderr.String(), "No evidence was recorded") {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if store.acquired != 1 || store.listed != 1 || store.inspected != 0 || len(store.resolved) != 0 {
		t.Fatalf("changed set performed evidence work: %#v", store)
	}
}

func TestExecutionRecoverAllApplyRequiresReviewedSetDigest(t *testing.T) {
	store := &fakeExecutionRecoveryStore{}
	var stdout, stderr bytes.Buffer
	code := handleExecutionRecover(store, "/workspace/repo", []string{
		"aaaaa11", "--all", "--apply", "--observation", "effect_not_applied",
		"--source", "verification_check", "--reference", "ref", "--summary", "sum",
		"--observed-at", "2026-07-14T10:00:00Z",
	}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "--set-digest from a prior inspection") {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	if store.acquired != 0 || store.listed != 0 {
		t.Fatalf("missing digest touched the store: %#v", store)
	}
}

func TestExecutionRecoverAllApplyRejectsPerExecutionTokens(t *testing.T) {
	store := &fakeExecutionRecoveryStore{}
	var stdout, stderr bytes.Buffer
	code := handleExecutionRecover(store, "/workspace/repo", []string{
		"aaaaa11", "--all", "--apply", "--revision", "4", "--event-id", "29",
		"--observation", "effect_not_applied", "--source", "verification_check",
		"--reference", "ref", "--summary", "sum", "--observed-at", "2026-07-14T10:00:00Z",
	}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "cannot be combined with --all") {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	if store.acquired != 0 || store.listed != 0 {
		t.Fatalf("rejected batch apply still touched the store: %#v", store)
	}
}

func TestExecutionRecoverApplyRequiresPriorInspectionTokens(t *testing.T) {
	store := &fakeExecutionRecoveryStore{}
	var stdout, stderr bytes.Buffer
	code := handleExecutionRecover(store, "/workspace/repo", []string{
		"--apply", "--observation", "effect_not_applied", "--source", "verification_check",
		"--reference", "check:absent", "--summary", "Verified effect absence.",
		"--observed-at", "2026-07-13T12:00:00Z", "aaaaa11", "exec_timeout",
	}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "--revision") || store.acquired != 0 {
		t.Fatalf("code=%d acquired=%d stdout=%q stderr=%q", code, store.acquired, stdout.String(), stderr.String())
	}
}

func TestNormalizeExecutionRecoverArgsKeepsInterspersedFlags(t *testing.T) {
	got, err := normalizeExecutionRecoverArgs([]string{"aaaaa11", "exec_timeout", "--revision", "4", "--event-id=29", "--apply"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--revision", "4", "--event-id=29", "--apply", "aaaaa11", "exec_timeout"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("normalized = %#v, want %#v", got, want)
	}
}
