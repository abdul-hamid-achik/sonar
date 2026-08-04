package db

import (
	"context"
	"fmt"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/controlplane"
)

// DismissCortexDecisionsSessionRequest couples terminal user-drop receipts to
// the exact session envelope that removes the corresponding local authority.
// Keeping both writes in one transaction prevents a dropped goal from leaving
// pending supervisor work, and prevents a failed session CAS from consuming
// the decision controls.
type DismissCortexDecisionsSessionRequest struct {
	SessionID               int64
	WorkspaceID             string
	ExpectedSessionRevision int64
	StateJSON               string
	Resolutions             []controlplane.Resolution
}

// DismissCortexDecisionsAndSaveSessionStateCAS appends only Cortex-decision
// dismissal receipts, then compare-and-swaps the session envelope in the same
// transaction. Execution reconciliation remains confined to its typed atomic
// coordinator.
func (s *Store) DismissCortexDecisionsAndSaveSessionStateCAS(
	ctx context.Context,
	lease *ExecutionSessionLease,
	request DismissCortexDecisionsSessionRequest,
) (SessionStateRecord, error) {
	if request.SessionID <= 0 {
		return SessionStateRecord{}, fmt.Errorf("invalid session id %d", request.SessionID)
	}
	if request.ExpectedSessionRevision < 0 {
		return SessionStateRecord{}, fmt.Errorf("invalid expected session state revision %d", request.ExpectedSessionRevision)
	}
	if len(request.Resolutions) == 0 || len(request.Resolutions) > controlplane.MaxListLimit {
		return SessionStateRecord{}, fmt.Errorf("cortex decision dismissal count must be between 1 and %d", controlplane.MaxListLimit)
	}
	seenItems := make(map[string]struct{}, len(request.Resolutions))
	for _, resolution := range request.Resolutions {
		if err := resolution.Validate(); err != nil {
			return SessionStateRecord{}, fmt.Errorf("validate Cortex decision dismissal: %w", err)
		}
		if resolution.SessionID != request.SessionID || resolution.WorkspaceID != request.WorkspaceID ||
			resolution.Outcome != controlplane.OutcomeDismissed {
			return SessionStateRecord{}, fmt.Errorf("cortex decision dismissal has inconsistent session scope or outcome")
		}
		if _, duplicate := seenItems[resolution.ItemID]; duplicate {
			return SessionStateRecord{}, fmt.Errorf("cortex decision dismissal item %q is duplicated", resolution.ItemID)
		}
		seenItems[resolution.ItemID] = struct{}{}
	}

	release, err := s.holdControlLease(ctx, lease, request.SessionID, request.WorkspaceID)
	if err != nil {
		return SessionStateRecord{}, err
	}
	defer release()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SessionStateRecord{}, fmt.Errorf("begin Cortex decision dismissal commit: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	storedResolutions := make([]controlplane.Resolution, 0, len(request.Resolutions))
	for _, resolution := range request.Resolutions {
		item, itemErr := getControlItemByItemID(ctx, tx, resolution.ItemID)
		if itemErr != nil {
			return SessionStateRecord{}, itemErr
		}
		if item.Kind != controlplane.KindCortexDecision ||
			item.Identity.SessionID != request.SessionID || item.Identity.WorkspaceID != request.WorkspaceID {
			return SessionStateRecord{}, fmt.Errorf("control item %q is not a Cortex decision in the requested scope", item.ItemID)
		}
		stored, _, resolveErr := resolveControlItemTx(ctx, tx, resolution, false)
		if resolveErr != nil {
			return SessionStateRecord{}, resolveErr
		}
		storedResolutions = append(storedResolutions, stored)
	}

	stateJSON := normalizedSessionStateJSON(request.StateJSON)
	record, err := saveSessionStateCASTx(ctx, tx, request.SessionID, request.ExpectedSessionRevision, stateJSON)
	if err != nil {
		return SessionStateRecord{}, err
	}
	if err := touchSessionTx(ctx, tx, request.SessionID); err != nil {
		return SessionStateRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		// A commit response can be lost after SQLite made the transaction
		// durable. Treat an exact read-back as success; otherwise surface the
		// failure without guessing or retrying caller-owned state.
		_ = tx.Rollback()
		if committedSessionState(s, request.SessionID, record) &&
			s.committedControlResolutions(request.SessionID, request.WorkspaceID, storedResolutions) {
			return record, nil
		}
		return SessionStateRecord{}, fmt.Errorf("commit Cortex decision dismissal and session state: %w", err)
	}
	return record, nil
}

func (s *Store) committedControlResolutions(sessionID int64, workspaceID string, expected []controlplane.Resolution) bool {
	readCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for _, resolution := range expected {
		state, err := s.GetControlState(readCtx, sessionID, workspaceID, resolution.ItemID)
		if err != nil || state.Resolution == nil || !controlResolutionsEquivalent(*state.Resolution, resolution) {
			return false
		}
	}
	return true
}
