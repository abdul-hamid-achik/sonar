package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/abdul-hamid-achik/sonar/internal/goal"
)

// ReconciliationGroupInspection is the bounded repository projection used to
// validate read-only recovery views. Group retains its canonical payload and
// evidence so trusted adapters can verify identities; operator surfaces must
// map it to a dedicated redacted presentation type before encoding or printing.
type ReconciliationGroupInspection struct {
	SessionID        int64
	SessionRevision  int64
	GoalID           string
	GoalState        goal.State
	TurnID           string
	BlockerReference string
	Group            ReconciliationGroup `json:"-"`
}

// InspectReconciliationGroup derives the exact recovery identity from the
// revisioned session and returns only an already-existing validated group. It
// always uses a read-only transaction and never calls Ensure, appends control
// items, resolves evidence, changes the session cursor, or acquires a lease.
//
// A completed reconciliation remains inspectable through the typed receipt in
// the paused/exhausted goal. This supports exact replay and audit without
// weakening the requirement that new recovery starts from an outcome-unknown
// blocker.
func (s *Store) InspectReconciliationGroup(ctx context.Context, sessionID int64, workspaceID string) (ReconciliationGroupInspection, error) {
	if err := validateExecutionSessionScope(ctx, s.db, sessionID, workspaceID); err != nil {
		return ReconciliationGroupInspection{}, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ReconciliationGroupInspection{}, fmt.Errorf("begin reconciliation inspection: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	record, err := getSessionStateRecord(ctx, tx, sessionID)
	if err != nil {
		return ReconciliationGroupInspection{}, err
	}
	session, err := decodeReconciliationSession(record)
	if err != nil {
		return ReconciliationGroupInspection{}, err
	}

	turnID, blockedErr := outcomeUnknownTurn(session.goal)
	if blockedErr == nil {
		group, err := getReconciliationGroupByScopeTx(ctx, tx, sessionID, workspaceID, session.goal.ID, turnID)
		if err != nil {
			return ReconciliationGroupInspection{}, err
		}
		if group.ParentResolution != nil {
			return ReconciliationGroupInspection{}, fmt.Errorf("%w: blocked goal already has a parent resolution", ErrReconciliationRepairRequired)
		}
		if err := validateGroupAgainstBlockedSession(group, session); err != nil {
			return ReconciliationGroupInspection{}, err
		}
		if err := rejectCompletedPostCursorTx(ctx, tx, sessionID, workspaceID, group.SnapshotCursor); err != nil {
			return ReconciliationGroupInspection{}, err
		}
		if err := verifyReconciliationGroupMembersTx(ctx, tx, &group, false); err != nil {
			return ReconciliationGroupInspection{}, err
		}
		if err := rejectOutsideGroupPostCursorHazardsTx(ctx, tx, group); err != nil {
			return ReconciliationGroupInspection{}, err
		}
		return newReconciliationGroupInspection(record, session.goal, group), nil
	}

	resolved := session.goal.LastBlockResolution
	if resolved == nil || resolved.Resolution.Reconciliation == nil ||
		(session.goal.State != goal.StatePaused && session.goal.State != goal.StateExhausted) {
		if errors.Is(blockedErr, goal.ErrOutcomeUnknown) {
			return ReconciliationGroupInspection{}, ErrReconciliationGroupNotFound
		}
		return ReconciliationGroupInspection{}, blockedErr
	}
	receipt := resolved.Resolution.Reconciliation
	group, err := getReconciliationGroupByItemIDTx(ctx, tx, receipt.GroupItemID)
	if err != nil {
		return ReconciliationGroupInspection{}, err
	}
	if group.SessionID != sessionID || group.WorkspaceID != workspaceID || group.GoalID != session.goal.ID || group.ParentResolution == nil {
		return ReconciliationGroupInspection{}, fmt.Errorf("%w: reconciled goal and group scope differ", ErrReconciliationRepairRequired)
	}
	if err := verifyReconciliationGroupMembersTx(ctx, tx, &group, true); err != nil {
		return ReconciliationGroupInspection{}, err
	}
	verified, err := verifiedFinalReplayReceipt(ctx, tx, group, session, record)
	if err != nil {
		return ReconciliationGroupInspection{}, err
	}
	if verified.GroupItemID != receipt.GroupItemID || verified.ResolutionID != receipt.FinalResolutionID {
		return ReconciliationGroupInspection{}, fmt.Errorf("%w: reconciled goal receipt differs from group", ErrReconciliationRepairRequired)
	}
	return newReconciliationGroupInspection(record, session.goal, group), nil
}

func newReconciliationGroupInspection(record SessionStateRecord, snapshot goal.Snapshot, group ReconciliationGroup) ReconciliationGroupInspection {
	return ReconciliationGroupInspection{
		SessionID: record.SessionID, SessionRevision: record.Revision,
		GoalID: snapshot.ID, GoalState: snapshot.State, TurnID: group.TurnID,
		BlockerReference: group.BlockerReference, Group: group,
	}
}
