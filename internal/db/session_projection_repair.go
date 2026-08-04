package db

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"unicode/utf8"

	"github.com/abdul-hamid-achik/sonar/internal/execution"
)

var (
	// ErrSessionProjectionReconcileFirst reports executions whose external
	// outcome is still unknown; repair must not skip evidence collection.
	ErrSessionProjectionReconcileFirst = errors.New("executions pending reconciliation must be resolved before projection repair")
	// ErrSessionProjectionCurrent means the durable cursor already covers every
	// execution event, so there is nothing to repair.
	ErrSessionProjectionCurrent = errors.New("session projection is already current")
)

// RepairedSessionEffect is one answered non-read-only execution whose terminal
// receipt was newer than the saved session snapshot at repair time. The
// bounded ledger receipt is the host-recorded, post-redaction content.
type RepairedSessionEffect struct {
	ExecutionID   string
	ToolName      string
	EventID       int64
	EventType     execution.EventType
	EffectClass   execution.EffectClass
	ResultReceipt string
}

// SessionProjectionRepairReceipt documents one committed projection repair.
// AnsweredTotal counts every answered effect the cursor advanced past;
// Repaired carries bounded detail for the first maxExecutionRecoveryHazards
// of them, so a receipt can never silently claim completeness it lacks.
type SessionProjectionRepairReceipt struct {
	SessionID       int64
	WorkspaceID     string
	SessionRevision int64
	PreviousCursor  int64
	NewCursor       int64
	AnsweredTotal   int
	Repaired        []RepairedSessionEffect
}

// RepairSessionProjection re-derives the session's execution snapshot cursor
// from durable ledger state after a crash left answered terminal receipts
// newer than the saved transcript. It is an explicit operator action: it
// refuses while any execution still requires reconciliation evidence, never
// retries a tool, and never rewrites the immutable execution ledger. The
// repaired effects are returned so the operator can see exactly which
// receipts the saved transcript is missing.
func (s *Store) RepairSessionProjection(ctx context.Context, lease *ExecutionSessionLease, sessionID int64, workspaceID string) (SessionProjectionRepairReceipt, error) {
	if sessionID <= 0 || workspaceID == "" {
		return SessionProjectionRepairReceipt{}, errors.New("session projection repair requires a session and workspace identity")
	}
	release, err := s.holdControlLease(ctx, lease, sessionID, workspaceID)
	if err != nil {
		return SessionProjectionRepairReceipt{}, err
	}
	defer release()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SessionProjectionRepairReceipt{}, fmt.Errorf("begin session projection repair: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := validateExecutionSessionScope(ctx, tx, sessionID, workspaceID); err != nil {
		return SessionProjectionRepairReceipt{}, err
	}
	record, err := getSessionStateRecord(ctx, tx, sessionID)
	if err != nil {
		return SessionProjectionRepairReceipt{}, err
	}
	// Goal-owned sessions repair through the goal recovery coordinator, which
	// owns their cursor and blocker lifecycle.
	if err := requireGoalLessSessionState(record.StateJSON); err != nil {
		return SessionProjectionRepairReceipt{}, err
	}
	cursor, err := decodeSessionExecutionCursor(record.StateJSON)
	if err != nil {
		return SessionProjectionRepairReceipt{}, err
	}
	latest, err := latestExecutionEventIDTx(ctx, tx, sessionID, workspaceID)
	if err != nil {
		return SessionProjectionRepairReceipt{}, err
	}
	if latest <= cursor {
		return SessionProjectionRepairReceipt{}, ErrSessionProjectionCurrent
	}
	// Page through EVERY raw candidate exactly like listEffectiveExecutionStates:
	// effectively-reconciled rows must not consume a bounded page and hide a
	// later unresolved execution, and truncation fails closed instead of
	// letting the cursor advance past unexamined state.
	query := effectiveExecutionQuery{
		kind: effectiveRecovery, sessionID: sessionID,
		workspaceID: workspaceID, afterEventID: cursor,
	}
	repaired := make([]RepairedSessionEffect, 0, maxExecutionRecoveryHazards)
	answeredTotal := 0
	offset := 0
	for {
		if offset >= maxEffectiveProjectionScan {
			return SessionProjectionRepairReceipt{}, fmt.Errorf("%w: scanned at least %d raw candidates", ErrExecutionHazardOverflow, maxEffectiveProjectionScan)
		}
		pageLimit := effectiveProjectionPageSize
		if remaining := maxEffectiveProjectionScan - offset; pageLimit > remaining {
			pageLimit = remaining
		}
		page, err := queryRawExecutionProjectionPage(ctx, tx, query, pageLimit, offset)
		if err != nil {
			return SessionProjectionRepairReceipt{}, err
		}
		if len(page) == 0 {
			break
		}
		offset += len(page)
		for _, state := range page {
			if executionStateCanBeReconciled(state) {
				reconciled, err := executionStateEffectivelyReconciled(ctx, tx, state)
				if err != nil {
					return SessionProjectionRepairReceipt{}, err
				}
				if reconciled {
					continue
				}
				return SessionProjectionRepairReceipt{}, fmt.Errorf(
					"%w: execution %q is %s; run `sonar execution recover %d --all` first",
					ErrSessionProjectionReconcileFirst, state.Identity.ExecutionID, state.Latest.Type, sessionID,
				)
			}
			if state.Latest.Type != execution.EventCompleted && state.Latest.Type != execution.EventFailed {
				return SessionProjectionRepairReceipt{}, fmt.Errorf(
					"%w: execution %q remains %s and cannot cross the snapshot boundary",
					ErrSessionProjectionReconcileFirst, state.Identity.ExecutionID, state.Latest.Type,
				)
			}
			answeredTotal++
			if len(repaired) < maxExecutionRecoveryHazards {
				repaired = append(repaired, RepairedSessionEffect{
					ExecutionID: state.Identity.ExecutionID, ToolName: state.Identity.ToolName,
					EventID: state.Latest.ID, EventType: state.Latest.Type,
					EffectClass: state.Identity.EffectClass, ResultReceipt: state.Latest.ResultReceipt,
				})
			}
		}
		if len(page) < pageLimit {
			break
		}
	}
	patched, err := patchTopLevelJSONObject([]byte(record.StateJSON), map[string][]byte{
		"execution_cursor": []byte(strconv.FormatInt(latest, 10)),
	})
	if err != nil {
		return SessionProjectionRepairReceipt{}, fmt.Errorf("patch session execution cursor: %w", err)
	}
	saved, err := saveSessionStateCASTx(ctx, tx, sessionID, record.Revision, patched)
	if err != nil {
		return SessionProjectionRepairReceipt{}, err
	}
	if err := tx.Commit(); err != nil {
		return SessionProjectionRepairReceipt{}, fmt.Errorf("commit session projection repair: %w", err)
	}
	return SessionProjectionRepairReceipt{
		SessionID: sessionID, WorkspaceID: workspaceID, SessionRevision: saved.Revision,
		PreviousCursor: cursor, NewCursor: latest,
		AnsweredTotal: answeredTotal, Repaired: repaired,
	}, nil
}

// decodeSessionExecutionCursor reads the versioned envelope's execution
// cursor without requiring a goal, mirroring decodeReconciliationSession's
// validation for standalone sessions.
func decodeSessionExecutionCursor(stateJSON string) (int64, error) {
	if !utf8.ValidString(stateJSON) || !json.Valid([]byte(stateJSON)) {
		return 0, errors.New("session state is not valid UTF-8 JSON")
	}
	if err := validateUniqueTopLevelJSONKeys([]byte(stateJSON)); err != nil {
		return 0, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stateJSON), &fields); err != nil {
		return 0, fmt.Errorf("decode session envelope: %w", err)
	}
	// The decode error and the range check used to share one branch, so a
	// version of "1", 1.5, true, or null reported "unsupported version 0" — a
	// value never legitimately persisted. session repair is a last-resort
	// recovery command; sending its operator after a compatibility problem
	// that does not exist costs real debugging time.
	var version int
	// An explicit null is an absent version, not a malformed number: it decodes
	// without error and leaves the zero value behind, which would otherwise be
	// reported as "unsupported version 0".
	raw := bytes.TrimSpace(fields["version"])
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return 0, errors.New("session envelope has no version")
	}
	if err := json.Unmarshal(raw, &version); err != nil {
		return 0, fmt.Errorf("decode session envelope version: %w", err)
	}
	if !supportedReconciliationEnvelopeVersion(version) {
		return 0, fmt.Errorf("unsupported session envelope version %d", version)
	}
	cursor := int64(0)
	if raw := fields["execution_cursor"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &cursor); err != nil {
			return 0, fmt.Errorf("decode session execution cursor: %w", err)
		}
	}
	if cursor < 0 {
		return 0, fmt.Errorf("session execution cursor is negative: %d", cursor)
	}
	return cursor, nil
}
