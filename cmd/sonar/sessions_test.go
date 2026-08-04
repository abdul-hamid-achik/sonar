package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/db"
	"github.com/abdul-hamid-achik/sonar/internal/execution"
	"github.com/abdul-hamid-achik/sonar/internal/sessionref"
)

type fakeSessionRepairStore struct {
	receipt   db.SessionProjectionRepairReceipt
	repairErr error
	leaseErr  error
	acquired  int
	repaired  int
	publicID  string
}

func (s *fakeSessionRepairStore) resolvePublicID() string {
	if s.publicID != "" {
		return s.publicID
	}
	switch s.receipt.SessionID {
	case 5:
		return "bbbbb05"
	default:
		return "bbbbb17"
	}
}

func (s *fakeSessionRepairStore) ResolveSessionRef(_ context.Context, ref string) (db.Session, error) {
	publicID, err := sessionref.Parse(ref)
	if err != nil {
		return db.Session{}, err
	}
	id := s.receipt.SessionID
	if id == 0 {
		id = 17
	}
	pid := s.resolvePublicID()
	if publicID != pid {
		// Error-path tests may resolve any valid handle before the lease fails.
		if s.leaseErr != nil || s.repairErr != nil {
			return db.Session{ID: id, PublicID: publicID, WorkspaceID: "/workspace/repo"}, nil
		}
		return db.Session{}, fmt.Errorf("session %s: not found", publicID)
	}
	return db.Session{ID: id, PublicID: pid, WorkspaceID: s.receipt.WorkspaceID}, nil
}

func (s *fakeSessionRepairStore) SessionHandle(_ context.Context, sessionID int64) (string, error) {
	return s.resolvePublicID(), nil
}

func (s *fakeSessionRepairStore) AcquireExecutionSessionLease(context.Context, int64, string) (*db.ExecutionSessionLease, error) {
	s.acquired++
	return nil, s.leaseErr
}

func (s *fakeSessionRepairStore) RepairSessionProjection(context.Context, *db.ExecutionSessionLease, int64, string) (db.SessionProjectionRepairReceipt, error) {
	s.repaired++
	return s.receipt, s.repairErr
}

func TestSessionRepairRendersBoundedReceipt(t *testing.T) {
	store := &fakeSessionRepairStore{receipt: db.SessionProjectionRepairReceipt{
		SessionID: 17, WorkspaceID: "/workspace/repo", SessionRevision: 9,
		PreviousCursor: 3, NewCursor: 41, AnsweredTotal: 2,
		Repaired: []db.RepairedSessionEffect{
			{
				ExecutionID: "exec_crash", ToolName: "bash", EventID: 40,
				EventType: execution.EventFailed, EffectClass: execution.EffectUnknown,
				ResultReceipt: "backend answered: exit status 7",
			},
			{
				ExecutionID: "exec_write", ToolName: "write", EventID: 41,
				EventType: execution.EventCompleted, EffectClass: execution.Effectful,
				ResultReceipt: "wrote file",
			},
		},
	}}
	var stdout, stderr bytes.Buffer
	code := handleSessionRepair(store, "/workspace/repo", []string{"bbbbb17", "--json"}, &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	if store.acquired != 1 || store.repaired != 1 {
		t.Fatalf("store calls = %#v", store)
	}
	for _, want := range []string{`"session_id": 17`, `"answered_total": 2`, `"exec_crash"`, `"receipt_on_file": true`, `"new_cursor": 41`} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("JSON receipt missing %q: %s", want, stdout.String())
		}
	}

	stdout.Reset()
	if code := handleSessionRepair(store, "/workspace/repo", []string{"--json", "bbbbb17"}, &stdout, &stderr); code != 0 {
		t.Fatalf("flag-first parse exit=%d stderr=%q", code, stderr.String())
	}

	stdout.Reset()
	if code := handleSessionRepair(store, "/workspace/repo", []string{"bbbbb17"}, &stdout, &stderr); code != 0 {
		t.Fatalf("text render exit=%d stderr=%q", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"Repaired session bbbbb17", "cursor 3 -> 41 @ revision 9", "2 answered effect(s)", "exec_crash", "failed/unknown",
		"absent from the saved transcript", "already terminal", "Review the durable",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("text receipt missing %q: %s", want, out)
		}
	}
	if strings.Contains(out, "sonar execution recover") || strings.Contains(out, "EXECUTION_ID") {
		t.Fatalf("text receipt recommends impossible terminal-effect recovery: %s", out)
	}
}

func TestSessionRepairReportsTruncatedTotalsAndCleanCursor(t *testing.T) {
	truncated := &fakeSessionRepairStore{receipt: db.SessionProjectionRepairReceipt{
		SessionID: 5, WorkspaceID: "/workspace/repo", SessionRevision: 2,
		PreviousCursor: 0, NewCursor: 900, AnsweredTotal: 107,
		Repaired: []db.RepairedSessionEffect{{
			ExecutionID: "exec_first", ToolName: "bash", EventID: 12,
			EventType: execution.EventCompleted, EffectClass: execution.Effectful,
		}},
	}}
	var stdout, stderr bytes.Buffer
	if code := handleSessionRepair(truncated, "/workspace/repo", []string{"bbbbb05"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit stderr=%q", stderr.String())
	}
	for _, want := range []string{"107 answered effect(s)", "...and 106 more", "durable execution ledger"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("truncated receipt missing %q: %s", want, stdout.String())
		}
	}

	clean := &fakeSessionRepairStore{receipt: db.SessionProjectionRepairReceipt{
		SessionID: 5, SessionRevision: 2, PreviousCursor: 3, NewCursor: 9,
	}}
	stdout.Reset()
	if code := handleSessionRepair(clean, "/workspace/repo", []string{"bbbbb05"}, &stdout, &stderr); code != 0 {
		t.Fatalf("clean repair exit stderr=%q", stderr.String())
	}
	if !strings.Contains(stdout.String(), "No answered effects were missing") {
		t.Fatalf("clean receipt = %s", stdout.String())
	}
}

func TestSessionRepairRejectsInvalidInputAndSurfacesStoreErrors(t *testing.T) {
	store := &fakeSessionRepairStore{}
	var stdout, stderr bytes.Buffer
	if code := handleSessionRepair(store, "/workspace/repo", nil, &stdout, &stderr); code != 2 {
		t.Fatalf("missing session id exit=%d", code)
	}
	if code := handleSessionRepair(store, "/workspace/repo", []string{"zero"}, &stdout, &stderr); code != 2 {
		t.Fatalf("invalid session id exit=%d", code)
	}
	if code := handleSessionRepair(store, "/workspace/repo", []string{"1", "2"}, &stdout, &stderr); code != 2 {
		t.Fatalf("extra positional exit=%d", code)
	}
	if store.acquired != 0 {
		t.Fatalf("rejected input touched the store: %#v", store)
	}

	busy := &fakeSessionRepairStore{leaseErr: errors.New("session lease is busy")}
	stderr.Reset()
	if code := handleSessionRepair(busy, "/workspace/repo", []string{"aaaaa11"}, &stdout, &stderr); code != 1 ||
		!strings.Contains(stderr.String(), "session lease is busy") {
		t.Fatalf("busy lease exit=%d stderr=%q", code, stderr.String())
	}

	refused := &fakeSessionRepairStore{repairErr: db.ErrSessionProjectionReconcileFirst}
	stderr.Reset()
	if code := handleSessionRepair(refused, "/workspace/repo", []string{"aaaaa11"}, &stdout, &stderr); code != 1 ||
		!strings.Contains(stderr.String(), "pending reconciliation") {
		t.Fatalf("reconcile-first exit=%d stderr=%q", code, stderr.String())
	}
}

func TestSessionCommandDispatchMatchesSiblings(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := handleSessionCommandIO(nil, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "session repair") {
		t.Fatalf("bare usage exit=%d out=%q", code, stdout.String())
	}
	stdout.Reset()
	if code := handleSessionCommandIO([]string{"help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("help exit=%d", code)
	}
	if code := handleSessionCommandIO([]string{"help", "extra"}, &stdout, &stderr); code != 2 {
		t.Fatalf("help with extra argument exit=%d", code)
	}
	stderr.Reset()
	if code := handleSessionCommandIO([]string{"unknown"}, &stdout, &stderr); code != 2 ||
		!strings.Contains(stderr.String(), "unknown command") {
		t.Fatalf("unknown subcommand exit=%d stderr=%q", code, stderr.String())
	}
}
