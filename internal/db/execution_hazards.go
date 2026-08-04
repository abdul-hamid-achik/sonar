package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/abdul-hamid-achik/sonar/internal/controlplane"
	"github.com/abdul-hamid-achik/sonar/internal/execution"
	"github.com/abdul-hamid-achik/sonar/internal/reconciliation"
)

var (
	ErrExecutionReconciliationCorrupt = errors.New("execution reconciliation projection is corrupt")
	ErrExecutionHazardOverflow        = errors.New("execution hazard projection exceeds its safe bound")
)

const (
	effectiveProjectionPageSize = 128
	maxEffectiveProjectionScan  = 10_000
)

// ExecutionRecoveryProjection is the bounded, validated restore-time view of
// an ordinary session's post-snapshot execution state. Hazards still block
// provider work. Contexts are closed host projections of exact typed
// reconciliations and contain no operator-authored free text.
type ExecutionRecoveryProjection struct {
	Hazards  []execution.State
	Contexts []StandaloneReconciliationContext
}

type effectiveExecutionKind uint8

const (
	effectiveUnresolved effectiveExecutionKind = iota + 1
	effectiveRecovery
	effectiveReconciliationTargets
	effectiveReconciliationPending
)

type effectiveExecutionQuery struct {
	kind            effectiveExecutionKind
	sessionID       int64
	workspaceID     string
	turnID          string
	afterEventID    int64
	requireGoalLess bool
}

// listEffectiveExecutionStates overlays typed control-plane reconciliation on
// the immutable raw ledger. It pages raw candidates and validates each exact
// receipt before skipping it, so reconciled rows cannot consume the caller's
// limit and hide a later unresolved execution.
func (s *Store) listEffectiveExecutionStates(ctx context.Context, query effectiveExecutionQuery, limit int, failOnOverflow bool) ([]execution.State, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin effective execution projection: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := validateExecutionSessionScope(ctx, tx, query.sessionID, query.workspaceID); err != nil {
		return nil, err
	}
	if query.requireGoalLess {
		record, err := getSessionStateRecord(ctx, tx, query.sessionID)
		if err != nil {
			return nil, err
		}
		if err := requireGoalLessSessionState(record.StateJSON); err != nil {
			return nil, err
		}
	}

	wanted := limit
	if failOnOverflow {
		wanted++
	}
	states := make([]execution.State, 0, wanted)
	offset := 0
	for len(states) < wanted {
		if offset >= maxEffectiveProjectionScan {
			return nil, fmt.Errorf("%w: scanned at least %d raw candidates", ErrExecutionHazardOverflow, maxEffectiveProjectionScan)
		}
		pageLimit := effectiveProjectionPageSize
		if remaining := maxEffectiveProjectionScan - offset; pageLimit > remaining {
			pageLimit = remaining
		}
		page, err := queryRawExecutionProjectionPage(ctx, tx, query, pageLimit, offset)
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			break
		}
		offset += len(page)
		for _, state := range page {
			if executionStateCanBeReconciled(state) {
				reconciled, err := executionStateEffectivelyReconciled(ctx, tx, state)
				if err != nil {
					return nil, err
				}
				if reconciled {
					continue
				}
			}
			states = append(states, state)
			if len(states) == wanted {
				break
			}
		}
		if len(page) < pageLimit {
			break
		}
	}
	if failOnOverflow && len(states) > limit {
		return nil, fmt.Errorf("%w: more than %d effective hazards", ErrExecutionHazardOverflow, limit)
	}
	if len(states) > limit {
		states = states[:limit]
	}
	return states, nil
}

// ProjectExecutionRecovery returns unresolved hazards together with validated
// standalone reconciliation contexts that the effective hazard view normally
// filters out. It is intentionally goal-less: goal recovery has a separate
// authority projection and must never be flattened into ordinary model
// context. The combined projection is bounded by limit and fails closed on
// overflow or corrupt evidence.
func (s *Store) ProjectExecutionRecovery(ctx context.Context, sessionID int64, workspaceID string, afterEventID int64, limit int) (ExecutionRecoveryProjection, error) {
	if afterEventID < 0 {
		return ExecutionRecoveryProjection{}, fmt.Errorf("execution recovery cursor must not be negative")
	}
	if err := validateExecutionListLimit(limit, maxExecutionRecoveryHazards); err != nil {
		return ExecutionRecoveryProjection{}, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ExecutionRecoveryProjection{}, fmt.Errorf("begin execution recovery projection: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := validateExecutionSessionScope(ctx, tx, sessionID, workspaceID); err != nil {
		return ExecutionRecoveryProjection{}, err
	}
	record, err := getSessionStateRecord(ctx, tx, sessionID)
	if err != nil {
		return ExecutionRecoveryProjection{}, err
	}
	if err := requireGoalLessSessionState(record.StateJSON); err != nil {
		return ExecutionRecoveryProjection{}, err
	}

	query := effectiveExecutionQuery{
		kind: effectiveRecovery, sessionID: sessionID, workspaceID: workspaceID,
		afterEventID: afterEventID,
	}
	projection := ExecutionRecoveryProjection{
		Hazards: make([]execution.State, 0, limit), Contexts: make([]StandaloneReconciliationContext, 0, limit),
	}
	offset := 0
	for {
		if offset >= maxEffectiveProjectionScan {
			return ExecutionRecoveryProjection{}, fmt.Errorf("%w: scanned at least %d raw candidates", ErrExecutionHazardOverflow, maxEffectiveProjectionScan)
		}
		pageLimit := effectiveProjectionPageSize
		if remaining := maxEffectiveProjectionScan - offset; pageLimit > remaining {
			pageLimit = remaining
		}
		page, err := queryRawExecutionProjectionPage(ctx, tx, query, pageLimit, offset)
		if err != nil {
			return ExecutionRecoveryProjection{}, err
		}
		if len(page) == 0 {
			break
		}
		offset += len(page)
		for _, state := range page {
			if executionStateCanBeReconciled(state) {
				validated, err := validatedExecutionReconciliationForState(ctx, tx, state)
				if err != nil {
					return ExecutionRecoveryProjection{}, err
				}
				if validated != nil {
					// Goal-owned receipts remain resolved for hazard filtering, but
					// only canonical goal-less evidence may enter ordinary context.
					if validated.envelope.GoalID == "" {
						expected, err := standaloneExecutionReconciliationItem(state)
						if err != nil {
							return ExecutionRecoveryProjection{}, err
						}
						if !controlItemsEquivalent(validated.state.Item, expected) {
							return ExecutionRecoveryProjection{}, fmt.Errorf("%w: execution %q has a non-canonical standalone control item", ErrExecutionReconciliationCorrupt, state.Identity.ExecutionID)
						}
						context, err := standaloneReconciliationContext(*validated.state.Resolution, validated.envelope, state)
						if err != nil {
							return ExecutionRecoveryProjection{}, fmt.Errorf("%w: execution %q host context: %v", ErrExecutionReconciliationCorrupt, state.Identity.ExecutionID, err)
						}
						projection.Contexts = append(projection.Contexts, context)
					}
					if len(projection.Hazards)+len(projection.Contexts) > limit {
						return ExecutionRecoveryProjection{}, fmt.Errorf("%w: more than %d restore entries", ErrExecutionHazardOverflow, limit)
					}
					continue
				}
			}
			projection.Hazards = append(projection.Hazards, state)
			if len(projection.Hazards)+len(projection.Contexts) > limit {
				return ExecutionRecoveryProjection{}, fmt.Errorf("%w: more than %d restore entries", ErrExecutionHazardOverflow, limit)
			}
		}
		if len(page) < pageLimit {
			break
		}
	}
	return projection, nil
}

func queryRawExecutionProjectionPage(ctx context.Context, tx *sql.Tx, query effectiveExecutionQuery, limit, offset int) ([]execution.State, error) {
	var statement string
	var args []any
	switch query.kind {
	case effectiveUnresolved:
		statement = `
			WITH ranked AS (
				SELECT e.*,
				       COUNT(*) OVER (PARTITION BY execution_id) AS event_count,
				       ROW_NUMBER() OVER (PARTITION BY execution_id ORDER BY id DESC) AS latest_rank
				  FROM execution_events e
				 WHERE session_id = ? AND workspace_id = ?
			)
			SELECT ` + executionEventColumns + `, event_count
			  FROM ranked
			 WHERE latest_rank = 1
			   AND event_type NOT IN ('denied', 'completed', 'failed', 'cancelled')
			 ORDER BY CASE
			              WHEN event_type = 'outcome_unknown' THEN 0
			              WHEN event_type = 'started' AND effect_class != 'read_only' THEN 0
			              ELSE 1
			          END,
			          id ASC
			 LIMIT ? OFFSET ?`
		args = []any{query.sessionID, query.workspaceID, limit, offset}
	case effectiveRecovery:
		statement = `
			WITH ranked AS (
				SELECT e.*,
				       COUNT(*) OVER (PARTITION BY execution_id) AS event_count,
				       ROW_NUMBER() OVER (PARTITION BY execution_id ORDER BY id DESC) AS latest_rank
				  FROM execution_events e
				 WHERE session_id = ? AND workspace_id = ?
			)
			SELECT ` + executionEventColumns + `, event_count
			  FROM ranked
			 WHERE latest_rank = 1
			   AND (
			       event_type = 'outcome_unknown'
			       OR (event_type = 'started' AND effect_class != 'read_only')
			       OR (event_type IN ('completed', 'failed') AND effect_class != 'read_only' AND id > ?)
			   )
			 ORDER BY CASE
			              WHEN event_type = 'outcome_unknown' THEN 0
			              WHEN event_type = 'started' AND effect_class != 'read_only' THEN 0
			              ELSE 1
			          END,
			          id ASC
			 LIMIT ? OFFSET ?`
		args = []any{query.sessionID, query.workspaceID, query.afterEventID, limit, offset}
	case effectiveReconciliationTargets:
		statement = `
			WITH ranked AS (
				SELECT e.*,
				       COUNT(*) OVER (PARTITION BY execution_id) AS event_count,
				       ROW_NUMBER() OVER (PARTITION BY execution_id ORDER BY id DESC) AS latest_rank
				  FROM execution_events e
				 WHERE session_id = ? AND workspace_id = ? AND turn_id = ?
			)
			SELECT ` + executionEventColumns + `, event_count
			  FROM ranked
			 WHERE latest_rank = 1
			   AND (
			       event_type = 'outcome_unknown'
			       OR (event_type = 'started' AND effect_class != 'read_only')
			   )
			 ORDER BY id ASC
			 LIMIT ? OFFSET ?`
		args = []any{query.sessionID, query.workspaceID, query.turnID, limit, offset}
	case effectiveReconciliationPending:
		statement = `
			WITH ranked AS (
				SELECT e.*,
				       COUNT(*) OVER (PARTITION BY execution_id) AS event_count,
				       ROW_NUMBER() OVER (PARTITION BY execution_id ORDER BY id DESC) AS latest_rank
				  FROM execution_events e
				 WHERE session_id = ? AND workspace_id = ?
			)
			SELECT ` + executionEventColumns + `, event_count
			  FROM ranked
			 WHERE latest_rank = 1
			   AND (
			       event_type = 'outcome_unknown'
			       OR (event_type = 'started' AND effect_class != 'read_only')
			   )
			 ORDER BY id ASC
			 LIMIT ? OFFSET ?`
		args = []any{query.sessionID, query.workspaceID, limit, offset}
	default:
		return nil, errors.New("invalid effective execution projection kind")
	}
	rows, err := tx.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("query raw execution projection: %w", err)
	}
	defer func() { _ = rows.Close() }()
	states := make([]execution.State, 0, limit)
	for rows.Next() {
		event, count, err := scanExecutionState(rows)
		if err != nil {
			return nil, err
		}
		states = append(states, execution.State{Identity: event.Identity, Latest: event, EventCount: count})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read raw execution projection: %w", err)
	}
	return states, nil
}

func executionStateCanBeReconciled(state execution.State) bool {
	return state.Latest.Type == execution.EventOutcomeUnknown ||
		(state.Latest.Type == execution.EventStarted && state.Identity.EffectClass != execution.EffectReadOnly)
}

type validatedExecutionReconciliation struct {
	state    controlplane.State
	envelope reconciliation.Envelope
}

func executionStateEffectivelyReconciled(ctx context.Context, tx *sql.Tx, state execution.State) (bool, error) {
	validated, err := validatedExecutionReconciliationForState(ctx, tx, state)
	return validated != nil, err
}

func validatedExecutionReconciliationForState(ctx context.Context, tx *sql.Tx, state execution.State) (*validatedExecutionReconciliation, error) {
	rows, err := tx.QueryContext(ctx, controlStateSelect+`
		WHERE i.session_id = ? AND i.workspace_id = ? AND i.execution_id = ?
		  AND i.kind = 'execution_reconciliation'
		ORDER BY i.id ASC
		LIMIT 2`, state.Identity.SessionID, state.Identity.WorkspaceID, state.Identity.ExecutionID)
	if err != nil {
		return nil, fmt.Errorf("query execution reconciliation overlay: %w", err)
	}
	controlStates := make([]controlplane.State, 0, 2)
	for rows.Next() {
		controlState, scanErr := scanControlState(rows)
		if scanErr != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("%w: scan control state: %v", ErrExecutionReconciliationCorrupt, scanErr)
		}
		controlStates = append(controlStates, controlState)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close execution reconciliation overlay: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read execution reconciliation overlay: %w", err)
	}
	if len(controlStates) == 0 {
		return nil, nil
	}
	if len(controlStates) != 1 {
		return nil, fmt.Errorf("%w: execution %q has %d control items", ErrExecutionReconciliationCorrupt, state.Identity.ExecutionID, len(controlStates))
	}
	controlState := controlStates[0]
	item := controlState.Item
	if err := item.Validate(); err != nil {
		return nil, fmt.Errorf("%w: invalid item %q: %v", ErrExecutionReconciliationCorrupt, item.ItemID, err)
	}
	if item.Identity.SessionID != state.Identity.SessionID ||
		item.Identity.WorkspaceID != state.Identity.WorkspaceID ||
		item.Identity.ExecutionID != state.Identity.ExecutionID ||
		item.Identity.TurnID != state.Identity.TurnID {
		return nil, fmt.Errorf("%w: item %q does not match the immutable execution scope", ErrExecutionReconciliationCorrupt, item.ItemID)
	}
	if controlState.Resolution == nil {
		return nil, nil
	}
	resolution := *controlState.Resolution
	if err := resolution.Validate(); err != nil {
		return nil, fmt.Errorf("%w: invalid resolution %q: %v", ErrExecutionReconciliationCorrupt, resolution.ResolutionID, err)
	}
	if resolution.ItemID != item.ItemID || resolution.SessionID != item.Identity.SessionID ||
		resolution.WorkspaceID != item.Identity.WorkspaceID || resolution.Outcome != controlplane.OutcomeReconciled {
		return nil, fmt.Errorf("%w: resolution %q does not exactly resolve item %q", ErrExecutionReconciliationCorrupt, resolution.ResolutionID, item.ItemID)
	}
	target, err := executionReconciliationTarget(item, state.Latest, resolution.ResolvedBy)
	if err != nil {
		return nil, fmt.Errorf("%w: derive target: %v", ErrExecutionReconciliationCorrupt, err)
	}
	envelope, err := reconciliation.Parse(resolution.EvidenceJSON, resolution.EvidenceSHA256)
	if err != nil {
		return nil, fmt.Errorf("%w: parse resolution %q: %v", ErrExecutionReconciliationCorrupt, resolution.ResolutionID, err)
	}
	if !envelope.MatchesTarget(target) {
		return nil, fmt.Errorf("%w: resolution %q target binding differs from durable state", ErrExecutionReconciliationCorrupt, resolution.ResolutionID)
	}
	return &validatedExecutionReconciliation{state: controlState, envelope: envelope}, nil
}
