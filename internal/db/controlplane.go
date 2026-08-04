package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/controlplane"
	"github.com/abdul-hamid-achik/sonar/internal/execution"
	"github.com/abdul-hamid-achik/sonar/internal/reconciliation"
)

var (
	ErrControlItemNotFound          = errors.New("control-plane item not found")
	ErrControlItemConflict          = errors.New("control-plane item conflict")
	ErrControlResolutionConflict    = errors.New("control-plane resolution conflict")
	ErrControlLeaseRequired         = errors.New("control-plane mutation requires an active session lease")
	ErrControlLeaseScope            = errors.New("control-plane lease does not own this session scope")
	ErrControlExecutionNotHazardous = errors.New("execution does not require outcome reconciliation")
	ErrControlReconciliationInvalid = errors.New("execution reconciliation evidence is invalid")
	ErrAtomicReconciliationRequired = errors.New("execution reconciliation requires the atomic session commit path")
)

const controlItemColumns = `
	id, item_id, idempotency_key, kind,
	session_id, workspace_id, goal_id, execution_id, turn_id,
	external_id, summary, payload_json, payload_sha256,
	created_at, recorded_at`

const controlResolutionColumns = `
	id, resolution_id, idempotency_key, item_id,
	session_id, workspace_id, outcome, evidence_json, evidence_sha256,
	resolved_by, detail, resolved_at, recorded_at`

// AppendControlItem records one immutable authority request. Every mutation
// requires possession of the exact session/workspace execution lease so a
// recovery process cannot race the live owner. Exact replays return the
// existing row with inserted=false; every differing collision fails closed.
func (s *Store) AppendControlItem(ctx context.Context, lease *ExecutionSessionLease, candidate controlplane.Item) (controlplane.Item, bool, error) {
	release, err := s.holdControlLease(ctx, lease, candidate.Identity.SessionID, candidate.Identity.WorkspaceID)
	if err != nil {
		return controlplane.Item{}, false, err
	}
	defer release()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return controlplane.Item{}, false, fmt.Errorf("begin control-plane item append: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	stored, inserted, err := appendControlItemTx(ctx, tx, candidate)
	if err != nil {
		return controlplane.Item{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return controlplane.Item{}, false, fmt.Errorf("commit control-plane item append: %w", err)
	}
	return stored, inserted, nil
}

// appendControlItemTx appends or exactly replays one item without acquiring a
// lease or committing the caller's transaction.
func appendControlItemTx(ctx context.Context, tx *sql.Tx, candidate controlplane.Item) (controlplane.Item, bool, error) {
	if err := candidate.Validate(); err != nil {
		return controlplane.Item{}, false, fmt.Errorf("validate control-plane item: %w", err)
	}
	if err := validateExecutionSessionScope(ctx, tx, candidate.Identity.SessionID, candidate.Identity.WorkspaceID); err != nil {
		return controlplane.Item{}, false, err
	}

	existing, err := queryControlItemsByIdentity(ctx, tx, candidate.ItemID, candidate.IdempotencyKey)
	if err != nil {
		return controlplane.Item{}, false, err
	}
	if len(existing) > 0 {
		if len(existing) == 1 && controlItemsEquivalent(existing[0], candidate) {
			return existing[0], false, nil
		}
		return controlplane.Item{}, false, fmt.Errorf("%w: item ID or idempotency key is bound to different immutable content", ErrControlItemConflict)
	}
	if candidate.Kind == controlplane.KindExecutionReconciliation {
		if _, err := validateReconcilableExecution(ctx, tx, candidate.Identity); err != nil {
			return controlplane.Item{}, false, err
		}
		var existingTarget string
		err := tx.QueryRowContext(ctx, `
			SELECT item_id
			  FROM control_items
			 WHERE session_id = ? AND workspace_id = ? AND execution_id = ?
			   AND kind = 'execution_reconciliation'
			 LIMIT 1`,
			candidate.Identity.SessionID, candidate.Identity.WorkspaceID, candidate.Identity.ExecutionID,
		).Scan(&existingTarget)
		if err == nil {
			return controlplane.Item{}, false, fmt.Errorf("%w: execution %q is already bound to item %q", ErrControlItemConflict, candidate.Identity.ExecutionID, existingTarget)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return controlplane.Item{}, false, fmt.Errorf("inspect execution reconciliation uniqueness: %w", err)
		}
	}
	if candidate.CreatedAt.IsZero() {
		candidate.CreatedAt = time.Now().UTC()
	} else {
		candidate.CreatedAt = candidate.CreatedAt.UTC()
	}

	result, err := tx.ExecContext(ctx, `
		INSERT INTO control_items (
			item_id, idempotency_key, session_id, workspace_id, kind,
			goal_id, execution_id, turn_id, external_id, summary,
			payload_json, payload_sha256, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		candidate.ItemID, candidate.IdempotencyKey,
		candidate.Identity.SessionID, candidate.Identity.WorkspaceID, candidate.Kind,
		candidate.Identity.GoalID, candidate.Identity.ExecutionID, candidate.Identity.TurnID,
		candidate.ExternalID, candidate.Summary, candidate.PayloadJSON, candidate.PayloadSHA256,
		formatExecutionTime(candidate.CreatedAt),
	)
	if err != nil {
		return controlplane.Item{}, false, fmt.Errorf("append control-plane item: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return controlplane.Item{}, false, fmt.Errorf("read control-plane item id: %w", err)
	}
	stored, err := getControlItemByRowID(ctx, tx, id)
	if err != nil {
		return controlplane.Item{}, false, err
	}
	return stored, true, nil
}

// ResolveControlItem appends the sole terminal resolution for an item. The
// parent item and the existing execution ledger remain untouched.
func (s *Store) ResolveControlItem(ctx context.Context, lease *ExecutionSessionLease, candidate controlplane.Resolution) (controlplane.Resolution, bool, error) {
	release, err := s.holdControlLease(ctx, lease, candidate.SessionID, candidate.WorkspaceID)
	if err != nil {
		return controlplane.Resolution{}, false, err
	}
	defer release()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return controlplane.Resolution{}, false, fmt.Errorf("begin control-plane resolution append: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	stored, inserted, err := resolveControlItemTx(ctx, tx, candidate, false)
	if err != nil {
		return controlplane.Resolution{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return controlplane.Resolution{}, false, fmt.Errorf("commit control-plane resolution append: %w", err)
	}
	return stored, inserted, nil
}

// resolveControlItemTx appends or replays one resolution inside the caller's
// transaction. It performs no lease acquisition and never commits, so the
// later reconciliation service can atomically couple this receipt to a
// session-state compare-and-swap.
func resolveControlItemTx(ctx context.Context, tx *sql.Tx, candidate controlplane.Resolution, allowExecutionReconciliation bool) (controlplane.Resolution, bool, error) {
	if err := candidate.Validate(); err != nil {
		return controlplane.Resolution{}, false, fmt.Errorf("validate control-plane resolution: %w", err)
	}
	if err := validateExecutionSessionScope(ctx, tx, candidate.SessionID, candidate.WorkspaceID); err != nil {
		return controlplane.Resolution{}, false, err
	}
	item, err := getControlItemByItemID(ctx, tx, candidate.ItemID)
	if errors.Is(err, ErrControlItemNotFound) {
		return controlplane.Resolution{}, false, err
	}
	if err != nil {
		return controlplane.Resolution{}, false, err
	}
	if item.Identity.SessionID != candidate.SessionID || item.Identity.WorkspaceID != candidate.WorkspaceID {
		return controlplane.Resolution{}, false, fmt.Errorf("%w: resolution scope differs from its immutable item", ErrControlResolutionConflict)
	}
	if !candidate.Outcome.ValidFor(item.Kind) {
		return controlplane.Resolution{}, false, fmt.Errorf("%w: outcome %q cannot resolve %s", ErrControlResolutionConflict, candidate.Outcome, item.Kind)
	}
	if item.Kind == controlplane.KindExecutionReconciliation && !allowExecutionReconciliation {
		return controlplane.Resolution{}, false, ErrAtomicReconciliationRequired
	}

	existing, err := queryControlResolutionsByIdentity(ctx, tx, candidate.ResolutionID, candidate.IdempotencyKey, candidate.ItemID)
	if err != nil {
		return controlplane.Resolution{}, false, err
	}
	if len(existing) > 0 {
		if len(existing) == 1 && controlResolutionsEquivalent(existing[0], candidate) {
			return existing[0], false, nil
		}
		return controlplane.Resolution{}, false, fmt.Errorf("%w: resolution ID, idempotency key, or item is bound to different immutable content", ErrControlResolutionConflict)
	}
	if item.Kind == controlplane.KindExecutionReconciliation {
		if err := validateExecutionReconciliationResolution(ctx, tx, item, candidate); err != nil {
			return controlplane.Resolution{}, false, err
		}
	}
	if candidate.ResolvedAt.IsZero() {
		candidate.ResolvedAt = time.Now().UTC()
	} else {
		candidate.ResolvedAt = candidate.ResolvedAt.UTC()
	}

	result, err := tx.ExecContext(ctx, `
		INSERT INTO control_resolutions (
			resolution_id, idempotency_key, item_id, session_id, workspace_id,
			outcome, evidence_json, evidence_sha256, resolved_by, detail, resolved_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		candidate.ResolutionID, candidate.IdempotencyKey, candidate.ItemID,
		candidate.SessionID, candidate.WorkspaceID, candidate.Outcome,
		candidate.EvidenceJSON, candidate.EvidenceSHA256,
		candidate.ResolvedBy, candidate.Detail, formatExecutionTime(candidate.ResolvedAt),
	)
	if err != nil {
		return controlplane.Resolution{}, false, fmt.Errorf("append control-plane resolution: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return controlplane.Resolution{}, false, fmt.Errorf("read control-plane resolution id: %w", err)
	}
	stored, err := getControlResolutionByRowID(ctx, tx, id)
	if err != nil {
		return controlplane.Resolution{}, false, err
	}
	return stored, true, nil
}

// GetControlState returns one item and its optional resolution inside the
// exact session/workspace scope.
func (s *Store) GetControlState(ctx context.Context, sessionID int64, workspaceID, itemID string) (controlplane.State, error) {
	if err := validateControlLookup(sessionID, workspaceID, itemID); err != nil {
		return controlplane.State{}, err
	}
	if err := validateExecutionSessionScope(ctx, s.db, sessionID, workspaceID); err != nil {
		return controlplane.State{}, err
	}
	state, err := scanControlState(s.db.QueryRowContext(ctx, controlStateSelect+`
		WHERE i.session_id = ? AND i.workspace_id = ? AND i.item_id = ?`,
		sessionID, workspaceID, itemID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return controlplane.State{}, ErrControlItemNotFound
	}
	if err != nil {
		return controlplane.State{}, fmt.Errorf("get control-plane state: %w", err)
	}
	return state, nil
}

// ListControlStates returns newest-first bounded state projections. Reads are
// observational and do not require the owner lease.
func (s *Store) ListControlStates(ctx context.Context, query controlplane.Query) ([]controlplane.State, error) {
	if err := query.Validate(); err != nil {
		return nil, fmt.Errorf("validate control-plane query: %w", err)
	}
	if err := validateExecutionSessionScope(ctx, s.db, query.SessionID, query.WorkspaceID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, controlStateSelect+`
		WHERE i.session_id = ? AND i.workspace_id = ?
		  AND (? = '' OR i.kind = ?)
		  AND (? = '' OR i.goal_id = ?)
		  AND (? = '' OR i.execution_id = ?)
		  AND (? = '' OR i.turn_id = ?)
		  AND (? = 0 OR r.id IS NULL)
		ORDER BY i.id DESC
		LIMIT ?`,
		query.SessionID, query.WorkspaceID,
		query.Kind, query.Kind,
		query.GoalID, query.GoalID,
		query.ExecutionID, query.ExecutionID,
		query.TurnID, query.TurnID,
		boolToInt(query.PendingOnly), query.Limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list control-plane states: %w", err)
	}
	defer func() { _ = rows.Close() }()

	states := make([]controlplane.State, 0)
	for rows.Next() {
		state, scanErr := scanControlState(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan control-plane state: %w", scanErr)
		}
		states = append(states, state)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read control-plane states: %w", err)
	}
	return states, nil
}

func (s *Store) holdControlLease(ctx context.Context, lease *ExecutionSessionLease, sessionID int64, workspaceID string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if lease == nil {
		return nil, ErrControlLeaseRequired
	}
	lease.mu.Lock()
	if lease.closed || lease.file == nil {
		lease.mu.Unlock()
		return nil, ErrControlLeaseRequired
	}
	if sessionID != lease.sessionID || workspaceID != lease.workspaceID ||
		s.executionLeaseRoot == "" || lease.leaseRoot != s.executionLeaseRoot {
		lease.mu.Unlock()
		return nil, fmt.Errorf("%w: lease session=%d workspace=%q", ErrControlLeaseScope, lease.sessionID, lease.workspaceID)
	}
	if err := ctx.Err(); err != nil {
		lease.mu.Unlock()
		return nil, err
	}
	return lease.mu.Unlock, nil
}

func validateReconcilableExecution(ctx context.Context, tx *sql.Tx, identity controlplane.Identity) (execution.Event, error) {
	event, err := scanExecutionEvent(tx.QueryRowContext(ctx, `
		SELECT `+executionEventColumns+`
		  FROM execution_events
		 WHERE session_id = ? AND workspace_id = ? AND execution_id = ?
		 ORDER BY id DESC LIMIT 1`,
		identity.SessionID, identity.WorkspaceID, identity.ExecutionID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return execution.Event{}, fmt.Errorf("%w: execution %q has no durable lifecycle", ErrControlExecutionNotHazardous, identity.ExecutionID)
	}
	if err != nil {
		return execution.Event{}, fmt.Errorf("read execution reconciliation target: %w", err)
	}
	if event.Identity.TurnID != identity.TurnID {
		return execution.Event{}, fmt.Errorf("%w: execution %q belongs to turn %q, not %q", ErrControlExecutionNotHazardous, identity.ExecutionID, event.Identity.TurnID, identity.TurnID)
	}
	if event.Type == execution.EventOutcomeUnknown ||
		(event.Type == execution.EventStarted && event.Identity.EffectClass != execution.EffectReadOnly) {
		return event, nil
	}
	return execution.Event{}, fmt.Errorf("%w: execution %q latest event is %s/%s", ErrControlExecutionNotHazardous, identity.ExecutionID, event.Type, event.Identity.EffectClass)
}

func validateExecutionReconciliationResolution(ctx context.Context, tx *sql.Tx, item controlplane.Item, candidate controlplane.Resolution) error {
	event, err := validateReconcilableExecution(ctx, tx, item.Identity)
	if err != nil {
		return err
	}
	target, err := executionReconciliationTarget(item, event, candidate.ResolvedBy)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrControlReconciliationInvalid, err)
	}
	envelope, err := reconciliation.Parse(candidate.EvidenceJSON, candidate.EvidenceSHA256)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrControlReconciliationInvalid, err)
	}
	if !envelope.MatchesTarget(target) {
		return fmt.Errorf("%w: evidence target differs from the durable item or latest event", ErrControlReconciliationInvalid)
	}
	return nil
}

func executionReconciliationTarget(item controlplane.Item, event execution.Event, actor string) (reconciliation.Target, error) {
	if item.Kind != controlplane.KindExecutionReconciliation ||
		item.Identity.SessionID != event.Identity.SessionID ||
		item.Identity.WorkspaceID != event.Identity.WorkspaceID ||
		item.Identity.ExecutionID != event.Identity.ExecutionID ||
		item.Identity.TurnID != event.Identity.TurnID {
		return reconciliation.Target{}, errors.New("control item does not match the immutable execution event")
	}
	eventDigest, err := executionEventDigest(event)
	if err != nil {
		return reconciliation.Target{}, err
	}
	target := reconciliation.Target{
		SessionID: item.Identity.SessionID, WorkspaceID: item.Identity.WorkspaceID,
		GoalID: item.Identity.GoalID, ItemID: item.ItemID,
		ItemPayloadSHA256: item.PayloadSHA256, ExecutionID: item.Identity.ExecutionID,
		TurnID: item.Identity.TurnID, LatestEventID: event.ID,
		LatestEventType: string(event.Type), LatestEventSHA256: eventDigest, Actor: actor,
	}
	if err := target.Validate(); err != nil {
		return reconciliation.Target{}, err
	}
	return target, nil
}

func executionEventDigest(event execution.Event) (string, error) {
	fingerprint := reconciliation.EventFingerprint{
		EventID: event.ID, SessionID: event.Identity.SessionID,
		WorkspaceID: event.Identity.WorkspaceID, RunID: event.Identity.RunID,
		ExecutionID: event.Identity.ExecutionID, IdempotencyKey: event.Identity.IdempotencyKey,
		TurnID: event.Identity.TurnID, CanonicalCallID: event.Identity.CanonicalCallID,
		ToolName: event.Identity.ToolName, Kind: string(event.Identity.Kind),
		Iteration: event.Identity.Iteration, Ordinal: event.Identity.Ordinal,
		EventType: string(event.Type), EffectClass: string(event.Identity.EffectClass),
		Approval: string(event.Approval), ArgumentsSHA256: event.ArgumentsSHA256,
		ResultSHA256: event.ResultSHA256, OccurredAt: event.OccurredAt,
	}
	return fingerprint.Digest()
}

func validateControlLookup(sessionID int64, workspaceID, itemID string) error {
	if sessionID <= 0 {
		return fmt.Errorf("invalid control-plane session id %d", sessionID)
	}
	if strings.TrimSpace(workspaceID) == "" {
		return errors.New("control-plane workspace id is required")
	}
	if strings.TrimSpace(itemID) == "" || len(itemID) > controlplane.MaxIdentityIDBytes {
		return errors.New("control-plane item id is required and must fit its bound")
	}
	return nil
}

func queryControlItemsByIdentity(ctx context.Context, tx *sql.Tx, itemID, idempotencyKey string) ([]controlplane.Item, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+controlItemColumns+`
		FROM control_items WHERE item_id = ? OR idempotency_key = ? ORDER BY id`, itemID, idempotencyKey)
	if err != nil {
		return nil, fmt.Errorf("query control-plane item identity: %w", err)
	}
	defer func() { _ = rows.Close() }()
	items := make([]controlplane.Item, 0, 2)
	for rows.Next() {
		item, scanErr := scanControlItem(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read control-plane item identity: %w", err)
	}
	return items, nil
}

func queryControlResolutionsByIdentity(ctx context.Context, tx *sql.Tx, resolutionID, idempotencyKey, itemID string) ([]controlplane.Resolution, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+controlResolutionColumns+`
		FROM control_resolutions
		WHERE resolution_id = ? OR idempotency_key = ? OR item_id = ?
		ORDER BY id`, resolutionID, idempotencyKey, itemID)
	if err != nil {
		return nil, fmt.Errorf("query control-plane resolution identity: %w", err)
	}
	defer func() { _ = rows.Close() }()
	resolutions := make([]controlplane.Resolution, 0, 3)
	for rows.Next() {
		resolution, scanErr := scanControlResolution(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		resolutions = append(resolutions, resolution)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read control-plane resolution identity: %w", err)
	}
	return resolutions, nil
}

func getControlItemByRowID(ctx context.Context, tx *sql.Tx, id int64) (controlplane.Item, error) {
	item, err := scanControlItem(tx.QueryRowContext(ctx, `SELECT `+controlItemColumns+` FROM control_items WHERE id = ?`, id))
	if err != nil {
		return controlplane.Item{}, fmt.Errorf("read appended control-plane item: %w", err)
	}
	return item, nil
}

func getControlItemByItemID(ctx context.Context, tx *sql.Tx, itemID string) (controlplane.Item, error) {
	item, err := scanControlItem(tx.QueryRowContext(ctx, `SELECT `+controlItemColumns+` FROM control_items WHERE item_id = ?`, itemID))
	if errors.Is(err, sql.ErrNoRows) {
		return controlplane.Item{}, ErrControlItemNotFound
	}
	if err != nil {
		return controlplane.Item{}, fmt.Errorf("read control-plane item: %w", err)
	}
	return item, nil
}

func getControlResolutionByRowID(ctx context.Context, tx *sql.Tx, id int64) (controlplane.Resolution, error) {
	resolution, err := scanControlResolution(tx.QueryRowContext(ctx, `SELECT `+controlResolutionColumns+` FROM control_resolutions WHERE id = ?`, id))
	if err != nil {
		return controlplane.Resolution{}, fmt.Errorf("read appended control-plane resolution: %w", err)
	}
	return resolution, nil
}

func controlItemsEquivalent(stored, candidate controlplane.Item) bool {
	if stored.ItemID != candidate.ItemID || stored.IdempotencyKey != candidate.IdempotencyKey ||
		stored.Kind != candidate.Kind || stored.Identity != candidate.Identity ||
		stored.ExternalID != candidate.ExternalID || stored.Summary != candidate.Summary ||
		stored.PayloadJSON != candidate.PayloadJSON || stored.PayloadSHA256 != candidate.PayloadSHA256 {
		return false
	}
	return candidate.CreatedAt.IsZero() || stored.CreatedAt.Equal(candidate.CreatedAt)
}

func controlResolutionsEquivalent(stored, candidate controlplane.Resolution) bool {
	if stored.ResolutionID != candidate.ResolutionID || stored.IdempotencyKey != candidate.IdempotencyKey ||
		stored.ItemID != candidate.ItemID || stored.SessionID != candidate.SessionID ||
		stored.WorkspaceID != candidate.WorkspaceID || stored.Outcome != candidate.Outcome ||
		stored.EvidenceJSON != candidate.EvidenceJSON || stored.EvidenceSHA256 != candidate.EvidenceSHA256 ||
		stored.ResolvedBy != candidate.ResolvedBy || stored.Detail != candidate.Detail {
		return false
	}
	return candidate.ResolvedAt.IsZero() || stored.ResolvedAt.Equal(candidate.ResolvedAt)
}

type controlRowScanner interface {
	Scan(dest ...any) error
}

func scanControlItem(scanner controlRowScanner) (controlplane.Item, error) {
	var item controlplane.Item
	var kind, createdAt, recordedAt string
	err := scanner.Scan(
		&item.ID, &item.ItemID, &item.IdempotencyKey, &kind,
		&item.Identity.SessionID, &item.Identity.WorkspaceID,
		&item.Identity.GoalID, &item.Identity.ExecutionID, &item.Identity.TurnID,
		&item.ExternalID, &item.Summary, &item.PayloadJSON, &item.PayloadSHA256,
		&createdAt, &recordedAt,
	)
	if err != nil {
		return controlplane.Item{}, err
	}
	item.Kind = controlplane.Kind(kind)
	item.CreatedAt, err = parseExecutionTime(createdAt)
	if err != nil {
		return controlplane.Item{}, fmt.Errorf("parse control-plane item time: %w", err)
	}
	item.RecordedAt, err = parseExecutionTime(recordedAt)
	if err != nil {
		return controlplane.Item{}, fmt.Errorf("parse control-plane item record time: %w", err)
	}
	return item, nil
}

func scanControlResolution(scanner controlRowScanner) (controlplane.Resolution, error) {
	var resolution controlplane.Resolution
	var outcome, resolvedAt, recordedAt string
	err := scanner.Scan(
		&resolution.ID, &resolution.ResolutionID, &resolution.IdempotencyKey, &resolution.ItemID,
		&resolution.SessionID, &resolution.WorkspaceID, &outcome,
		&resolution.EvidenceJSON, &resolution.EvidenceSHA256,
		&resolution.ResolvedBy, &resolution.Detail, &resolvedAt, &recordedAt,
	)
	if err != nil {
		return controlplane.Resolution{}, err
	}
	resolution.Outcome = controlplane.Outcome(outcome)
	resolution.ResolvedAt, err = parseExecutionTime(resolvedAt)
	if err != nil {
		return controlplane.Resolution{}, fmt.Errorf("parse control-plane resolution time: %w", err)
	}
	resolution.RecordedAt, err = parseExecutionTime(recordedAt)
	if err != nil {
		return controlplane.Resolution{}, fmt.Errorf("parse control-plane resolution record time: %w", err)
	}
	return resolution, nil
}

const controlStateSelect = `
	SELECT
		i.id, i.item_id, i.idempotency_key, i.kind,
		i.session_id, i.workspace_id, i.goal_id, i.execution_id, i.turn_id,
		i.external_id, i.summary, i.payload_json, i.payload_sha256,
		i.created_at, i.recorded_at,
		r.id, r.resolution_id, r.idempotency_key, r.item_id,
		r.session_id, r.workspace_id, r.outcome, r.evidence_json, r.evidence_sha256,
		r.resolved_by, r.detail, r.resolved_at, r.recorded_at
	FROM control_items i
	LEFT JOIN control_resolutions r ON r.item_id = i.item_id
`

func scanControlState(scanner controlRowScanner) (controlplane.State, error) {
	var state controlplane.State
	var itemKind, itemCreatedAt, itemRecordedAt string
	var resolutionID sql.NullInt64
	var resolutionIDText, resolutionIdempotencyKey, resolutionItemID sql.NullString
	var resolutionSessionID sql.NullInt64
	var resolutionWorkspaceID, outcome, evidenceJSON, evidenceSHA256 sql.NullString
	var resolvedBy, detail, resolvedAt, resolutionRecordedAt sql.NullString
	err := scanner.Scan(
		&state.Item.ID, &state.Item.ItemID, &state.Item.IdempotencyKey, &itemKind,
		&state.Item.Identity.SessionID, &state.Item.Identity.WorkspaceID,
		&state.Item.Identity.GoalID, &state.Item.Identity.ExecutionID, &state.Item.Identity.TurnID,
		&state.Item.ExternalID, &state.Item.Summary, &state.Item.PayloadJSON, &state.Item.PayloadSHA256,
		&itemCreatedAt, &itemRecordedAt,
		&resolutionID, &resolutionIDText, &resolutionIdempotencyKey, &resolutionItemID,
		&resolutionSessionID, &resolutionWorkspaceID, &outcome, &evidenceJSON, &evidenceSHA256,
		&resolvedBy, &detail, &resolvedAt, &resolutionRecordedAt,
	)
	if err != nil {
		return controlplane.State{}, err
	}
	state.Item.Kind = controlplane.Kind(itemKind)
	state.Item.CreatedAt, err = parseExecutionTime(itemCreatedAt)
	if err != nil {
		return controlplane.State{}, err
	}
	state.Item.RecordedAt, err = parseExecutionTime(itemRecordedAt)
	if err != nil {
		return controlplane.State{}, err
	}
	if !resolutionID.Valid {
		return state, nil
	}
	resolution := controlplane.Resolution{
		ID: resolutionID.Int64, ResolutionID: resolutionIDText.String,
		IdempotencyKey: resolutionIdempotencyKey.String, ItemID: resolutionItemID.String,
		SessionID: resolutionSessionID.Int64, WorkspaceID: resolutionWorkspaceID.String,
		Outcome: controlplane.Outcome(outcome.String), EvidenceJSON: evidenceJSON.String,
		EvidenceSHA256: evidenceSHA256.String, ResolvedBy: resolvedBy.String, Detail: detail.String,
	}
	resolution.ResolvedAt, err = parseExecutionTime(resolvedAt.String)
	if err != nil {
		return controlplane.State{}, err
	}
	resolution.RecordedAt, err = parseExecutionTime(resolutionRecordedAt.String)
	if err != nil {
		return controlplane.State{}, err
	}
	state.Resolution = &resolution
	return state, nil
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
