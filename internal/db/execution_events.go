package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/abdul-hamid-achik/sonar/internal/execution"
)

var (
	ErrExecutionNotFound          = errors.New("execution not found")
	ErrExecutionWorkspaceMismatch = errors.New("execution workspace does not match session")
	ErrExecutionIdentityConflict  = errors.New("execution identity conflict")
	ErrExecutionEventConflict     = errors.New("execution event conflict")
	ErrIllegalExecutionTransition = errors.New("illegal execution transition")
)

const (
	maxExecutionEventList       = 1000
	maxUnresolvedList           = 100
	maxExecutionRecoveryHazards = 100
)

const executionEventColumns = `
	id, session_id, workspace_id, run_id, turn_id, execution_id,
	idempotency_key, provider_call_id, canonical_call_id, iteration,
	ordinal, tool_name, kind, effect_class, event_type, approval,
	arguments_sha256, result_sha256, result_receipt, detail,
	occurred_at, recorded_at`

// AppendExecutionEvent appends one immutable lifecycle edge. Replaying the
// exact same execution/type pair is idempotent; any differing replay fails.
func (s *Store) AppendExecutionEvent(ctx context.Context, candidate execution.Event) (execution.Event, bool, error) {
	if candidate.Approval == "" {
		candidate.Approval = execution.ApprovalNotApplicable
	}
	if err := candidate.Validate(); err != nil {
		return execution.Event{}, false, fmt.Errorf("validate execution event: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return execution.Event{}, false, fmt.Errorf("begin execution event append: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := validateExecutionSessionScope(ctx, tx, candidate.Identity.SessionID, candidate.Identity.WorkspaceID); err != nil {
		return execution.Event{}, false, err
	}

	existing, err := queryExecutionLifecycle(ctx, tx, candidate.Identity.ExecutionID, candidate.Identity.IdempotencyKey)
	if err != nil {
		return execution.Event{}, false, err
	}
	for _, stored := range existing {
		if stored.Identity != candidate.Identity {
			return execution.Event{}, false, fmt.Errorf("%w: execution or idempotency key is already bound to different immutable metadata", ErrExecutionIdentityConflict)
		}
		if stored.Type == candidate.Type {
			if executionEventsEquivalent(stored, candidate) {
				if err := tx.Commit(); err != nil {
					return execution.Event{}, false, fmt.Errorf("commit execution event replay: %w", err)
				}
				return stored, false, nil
			}
			return execution.Event{}, false, fmt.Errorf("%w: %s event differs from the immutable stored row", ErrExecutionEventConflict, candidate.Type)
		}
	}

	if err := validateExecutionArgumentContinuity(existing, candidate); err != nil {
		return execution.Event{}, false, err
	}
	if err := validateExecutionTransition(existing, candidate); err != nil {
		return execution.Event{}, false, err
	}
	if candidate.Type == execution.EventRequested {
		if err := rejectExecutionRequestCollision(ctx, tx, candidate.Identity); err != nil {
			return execution.Event{}, false, err
		}
	}
	if candidate.OccurredAt.IsZero() {
		candidate.OccurredAt = time.Now().UTC()
	} else {
		candidate.OccurredAt = candidate.OccurredAt.UTC()
	}

	result, err := tx.ExecContext(ctx, `
		INSERT INTO execution_events (
			session_id, workspace_id, run_id, turn_id, execution_id,
			idempotency_key, provider_call_id, canonical_call_id, iteration,
			ordinal, tool_name, kind, effect_class, event_type, approval,
			arguments_sha256, result_sha256, result_receipt, detail, occurred_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		candidate.Identity.SessionID,
		candidate.Identity.WorkspaceID,
		candidate.Identity.RunID,
		candidate.Identity.TurnID,
		candidate.Identity.ExecutionID,
		candidate.Identity.IdempotencyKey,
		candidate.Identity.ProviderCallID,
		candidate.Identity.CanonicalCallID,
		candidate.Identity.Iteration,
		candidate.Identity.Ordinal,
		candidate.Identity.ToolName,
		candidate.Identity.Kind,
		candidate.Identity.EffectClass,
		candidate.Type,
		candidate.Approval,
		candidate.ArgumentsSHA256,
		candidate.ResultSHA256,
		candidate.ResultReceipt,
		candidate.Detail,
		formatExecutionTime(candidate.OccurredAt),
	)
	if err != nil {
		return execution.Event{}, false, fmt.Errorf("append execution event: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return execution.Event{}, false, fmt.Errorf("read execution event id: %w", err)
	}
	stored, err := getExecutionEventByID(ctx, tx, id)
	if err != nil {
		return execution.Event{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return execution.Event{}, false, fmt.Errorf("commit execution event append: %w", err)
	}
	return stored, true, nil
}

// GetExecutionState derives the latest read model for one scoped execution.
func (s *Store) GetExecutionState(ctx context.Context, sessionID int64, workspaceID, executionID string) (execution.State, error) {
	if err := validateExecutionSessionScope(ctx, s.db, sessionID, workspaceID); err != nil {
		return execution.State{}, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+executionEventColumns+`
		   FROM execution_events
		  WHERE session_id = ? AND workspace_id = ? AND execution_id = ?
		  ORDER BY id ASC`,
		sessionID, workspaceID, executionID,
	)
	if err != nil {
		return execution.State{}, fmt.Errorf("query execution state: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var state execution.State
	for rows.Next() {
		event, scanErr := scanExecutionEvent(rows)
		if scanErr != nil {
			return execution.State{}, scanErr
		}
		if state.EventCount == 0 {
			state.Identity = event.Identity
		}
		state.Latest = event
		state.EventCount++
	}
	if err := rows.Err(); err != nil {
		return execution.State{}, fmt.Errorf("read execution state: %w", err)
	}
	if state.EventCount == 0 {
		return execution.State{}, ErrExecutionNotFound
	}
	return state, nil
}

// ListExecutionEvents returns one lifecycle in durable insertion order.
func (s *Store) ListExecutionEvents(ctx context.Context, sessionID int64, workspaceID, executionID string, limit int) ([]execution.Event, error) {
	if err := validateExecutionListLimit(limit, maxExecutionEventList); err != nil {
		return nil, err
	}
	if err := validateExecutionSessionScope(ctx, s.db, sessionID, workspaceID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+executionEventColumns+`
		   FROM execution_events
		  WHERE session_id = ? AND workspace_id = ? AND execution_id = ?
		  ORDER BY id ASC LIMIT ?`,
		sessionID, workspaceID, executionID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list execution events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	events := make([]execution.Event, 0, 8)
	for rows.Next() {
		event, scanErr := scanExecutionEvent(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read execution events: %w", err)
	}
	return events, nil
}

// ListSessionExecutionEvents returns immutable lifecycle edges recorded for one
// session across all executions, in append order. It is the audit timeline for
// `session export`. When a session has more than limit events the MOST RECENT
// limit are returned (still chronological): an audit of a wedged session cares
// about the events that led to the wedge, not the session's first prompts.
func (s *Store) ListSessionExecutionEvents(ctx context.Context, sessionID int64, workspaceID string, limit int) ([]execution.Event, error) {
	if err := validateExecutionListLimit(limit, maxExecutionEventList); err != nil {
		return nil, err
	}
	if err := validateExecutionSessionScope(ctx, s.db, sessionID, workspaceID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+executionEventColumns+`
		   FROM (
		     SELECT `+executionEventColumns+`
		       FROM execution_events
		      WHERE session_id = ? AND workspace_id = ?
		      ORDER BY id DESC LIMIT ?
		   ) ORDER BY id ASC`,
		sessionID, workspaceID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list session execution events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	events := make([]execution.Event, 0, 32)
	for rows.Next() {
		event, scanErr := scanExecutionEvent(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read session execution events: %w", err)
	}
	return events, nil
}

// ApprovalPromptStat is one aggregated row of `/permissions audit`: how often
// one (tool, refusal detail) pair interrupted the operator in a workspace.
// Detail is the host-written approval_requested projection — for bash it names
// the scoped-shell rule that refused and, when one is derivable, the exact
// grant prefix that would cure it — never raw arguments.
type ApprovalPromptStat struct {
	ToolName string
	Detail   string
	Count    int64
	LastAt   string
}

// WorkspaceApprovalPromptStats aggregates approval_requested events across
// every session of one workspace, most-interrupting first. This is the audit
// loop that used to live in engineering-log comments keyed to session hashes:
// the rows name what keeps prompting, so the operator can decide what deserves
// a durable rule or a catalog entry instead of re-living the interruptions.
func (s *Store) WorkspaceApprovalPromptStats(ctx context.Context, workspaceID string, limit int) ([]ApprovalPromptStat, error) {
	if err := validateExecutionListLimit(limit, maxExecutionEventList); err != nil {
		return nil, err
	}
	if strings.TrimSpace(workspaceID) == "" {
		return nil, errors.New("workspace id is required")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT tool_name, detail, COUNT(*) AS prompts, MAX(occurred_at) AS last_at
		   FROM execution_events
		  WHERE workspace_id = ? AND event_type = 'approval_requested'
		  GROUP BY tool_name, detail
		  ORDER BY prompts DESC, last_at DESC
		  LIMIT ?`,
		workspaceID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("aggregate approval prompts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	stats := make([]ApprovalPromptStat, 0, 8)
	for rows.Next() {
		var stat ApprovalPromptStat
		if scanErr := rows.Scan(&stat.ToolName, &stat.Detail, &stat.Count, &stat.LastAt); scanErr != nil {
			return nil, fmt.Errorf("scan approval prompt stat: %w", scanErr)
		}
		stats = append(stats, stat)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read approval prompt stats: %w", err)
	}
	return stats, nil
}

// SessionExecutionEventExists reports whether eventID is a durable event in
// the exact scoped session. A recovery cursor is a global execution_events row
// ID, so checking only that it is below the session's latest ID can accidentally
// accept an event owned by another session.
func (s *Store) SessionExecutionEventExists(ctx context.Context, sessionID int64, workspaceID string, eventID int64) (bool, error) {
	if eventID <= 0 {
		return false, fmt.Errorf("invalid execution event id %d", eventID)
	}
	if err := validateExecutionSessionScope(ctx, s.db, sessionID, workspaceID); err != nil {
		return false, err
	}
	var exists bool
	if err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS(
		   SELECT 1
		     FROM execution_events
		    WHERE id = ? AND session_id = ? AND workspace_id = ?
		 )`,
		eventID, sessionID, workspaceID,
	).Scan(&exists); err != nil {
		return false, fmt.Errorf("check scoped execution event: %w", err)
	}
	return exists, nil
}

// ListUnresolvedExecutions returns bounded latest states that block automatic
// continuation. This includes non-terminal lifecycles and the durable terminal
// outcome_unknown state, which remains operationally unresolved until explicit
// reconciliation. It is deliberately read-only: recovery may not append a
// decision without an explicit session/run lease.
func (s *Store) ListUnresolvedExecutions(ctx context.Context, sessionID int64, workspaceID string, limit int) ([]execution.State, error) {
	if err := validateExecutionListLimit(limit, maxUnresolvedList); err != nil {
		return nil, err
	}
	return s.listEffectiveExecutionStates(ctx, effectiveExecutionQuery{
		kind: effectiveUnresolved, sessionID: sessionID, workspaceID: workspaceID,
	}, limit, false)
}

// LatestExecutionEventID returns the highest durable event ID in one scoped
// session, or zero when that session has no execution events. A completed-turn
// snapshot records this value as its recovery cursor.
func (s *Store) LatestExecutionEventID(ctx context.Context, sessionID int64, workspaceID string) (int64, error) {
	if err := validateExecutionSessionScope(ctx, s.db, sessionID, workspaceID); err != nil {
		return 0, err
	}
	var latest int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(id), 0)
		   FROM execution_events
		  WHERE session_id = ? AND workspace_id = ?`,
		sessionID, workspaceID,
	).Scan(&latest); err != nil {
		return 0, fmt.Errorf("read latest execution event id: %w", err)
	}
	return latest, nil
}

// ListExecutionRecoveryHazards returns the bounded latest states that must be
// reconciled with a restored snapshot. Outcome-unknown and started non-read-only
// executions are hazards regardless of the snapshot cursor. Completed and failed
// non-read-only executions are hazards only when their terminal receipt is newer
// than afterEventID and therefore may be absent from the snapshot projection.
func (s *Store) ListExecutionRecoveryHazards(ctx context.Context, sessionID int64, workspaceID string, afterEventID int64, limit int) ([]execution.State, error) {
	if afterEventID < 0 {
		return nil, fmt.Errorf("execution recovery cursor must not be negative")
	}
	if err := validateExecutionListLimit(limit, maxExecutionRecoveryHazards); err != nil {
		return nil, err
	}
	return s.listEffectiveExecutionStates(ctx, effectiveExecutionQuery{
		kind: effectiveRecovery, sessionID: sessionID, workspaceID: workspaceID,
		afterEventID: afterEventID,
	}, limit, true)
}

// ListExecutionReconciliationTargets returns the exact turn's bounded latest
// executions whose external outcome still requires evidence. Unlike the
// session recovery projection, it excludes completed post-cursor receipts:
// those may need snapshot projection, but their backend outcome is known and
// must not be represented as execution reconciliation.
// ListStandaloneExecutionReconciliationPending returns the session's bounded
// latest executions whose external outcome still requires evidence, oldest
// first, across every turn. Already-reconciled executions and answered
// post-cursor receipts are excluded: the former have evidence on file and the
// latter need snapshot projection, not reconciliation. It fails closed rather
// than truncating when more than limit effective pending executions exist.
func (s *Store) ListStandaloneExecutionReconciliationPending(ctx context.Context, sessionID int64, workspaceID string, limit int) ([]execution.State, error) {
	if err := validateExecutionListLimit(limit, maxExecutionRecoveryHazards); err != nil {
		return nil, err
	}
	return s.listEffectiveExecutionStates(ctx, effectiveExecutionQuery{
		kind: effectiveReconciliationPending, sessionID: sessionID, workspaceID: workspaceID,
		requireGoalLess: true,
	}, limit, true)
}

func (s *Store) ListExecutionReconciliationTargets(ctx context.Context, sessionID int64, workspaceID, turnID string, limit int) ([]execution.State, error) {
	if strings.TrimSpace(turnID) == "" || strings.TrimSpace(turnID) != turnID || len(turnID) > execution.MaxTurnIDBytes || !utf8.ValidString(turnID) {
		return nil, fmt.Errorf("execution reconciliation turn id is invalid")
	}
	if err := validateExecutionListLimit(limit, maxExecutionRecoveryHazards); err != nil {
		return nil, err
	}
	return s.listEffectiveExecutionStates(ctx, effectiveExecutionQuery{
		kind: effectiveReconciliationTargets, sessionID: sessionID,
		workspaceID: workspaceID, turnID: turnID,
	}, limit, true)
}

type executionRowScanner interface {
	Scan(dest ...any) error
}

type executionScopeQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func validateExecutionSessionScope(ctx context.Context, q executionScopeQuerier, sessionID int64, workspaceID string) error {
	if sessionID <= 0 {
		return fmt.Errorf("invalid execution session id %d", sessionID)
	}
	if strings.TrimSpace(workspaceID) == "" {
		return fmt.Errorf("execution workspace id is required")
	}
	var actual string
	err := q.QueryRowContext(ctx, `SELECT workspace_id FROM sessions WHERE id = ?`, sessionID).Scan(&actual)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: session %d", ErrExecutionNotFound, sessionID)
	}
	if err != nil {
		return fmt.Errorf("read execution session scope: %w", err)
	}
	if actual != workspaceID {
		return fmt.Errorf("%w: session %d belongs to %q, not %q", ErrExecutionWorkspaceMismatch, sessionID, actual, workspaceID)
	}
	return nil
}

func queryExecutionLifecycle(ctx context.Context, tx *sql.Tx, executionID, idempotencyKey string) ([]execution.Event, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT `+executionEventColumns+`
		   FROM execution_events
		  WHERE execution_id = ? OR idempotency_key = ?
		  ORDER BY id ASC`, executionID, idempotencyKey)
	if err != nil {
		return nil, fmt.Errorf("query execution lifecycle: %w", err)
	}
	defer func() { _ = rows.Close() }()

	events := make([]execution.Event, 0, 8)
	for rows.Next() {
		event, scanErr := scanExecutionEvent(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read execution lifecycle: %w", err)
	}
	return events, nil
}

func rejectExecutionRequestCollision(ctx context.Context, tx *sql.Tx, identity execution.Identity) error {
	var existingID string
	err := tx.QueryRowContext(ctx, `
		SELECT execution_id
		  FROM execution_events
		 WHERE event_type = 'requested'
		   AND session_id = ? AND run_id = ?
		   AND (
		       canonical_call_id = ?
		       OR (turn_id = ? AND iteration = ? AND ordinal = ?)
		   )
		 LIMIT 1`,
		identity.SessionID, identity.RunID, identity.CanonicalCallID,
		identity.TurnID, identity.Iteration, identity.Ordinal,
	).Scan(&existingID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("check execution request identity: %w", err)
	}
	return fmt.Errorf("%w: call or run position is already bound to execution %q", ErrExecutionIdentityConflict, existingID)
}

func validateExecutionArgumentContinuity(existing []execution.Event, candidate execution.Event) error {
	if candidate.Type == execution.EventRequested {
		return nil
	}
	var effective string
	for _, event := range existing {
		if event.Type == execution.EventRequested {
			continue
		}
		if effective == "" {
			effective = event.ArgumentsSHA256
			continue
		}
		if effective != event.ArgumentsSHA256 {
			return fmt.Errorf("%w: stored effective argument hashes disagree", ErrExecutionEventConflict)
		}
	}
	if effective != "" && effective != candidate.ArgumentsSHA256 {
		return fmt.Errorf("%w: effective argument hash changed after approval processing", ErrExecutionEventConflict)
	}
	return nil
}

func validateExecutionTransition(existing []execution.Event, candidate execution.Event) error {
	if len(existing) == 0 {
		if candidate.Type != execution.EventRequested {
			return fmt.Errorf("%w: first event must be requested, got %s", ErrIllegalExecutionTransition, candidate.Type)
		}
		if candidate.Approval != execution.ApprovalNotApplicable {
			return fmt.Errorf("%w: requested event approval must be not_applicable", ErrIllegalExecutionTransition)
		}
		return nil
	}

	requested := false
	approvalRequested := false
	approved := false
	started := false
	terminal := false
	for _, event := range existing {
		switch event.Type {
		case execution.EventRequested:
			requested = true
		case execution.EventApprovalRequested:
			approvalRequested = true
		case execution.EventApproved:
			approved = true
		case execution.EventStarted:
			started = true
		}
		terminal = terminal || event.Type.Terminal()
	}
	if !requested {
		return fmt.Errorf("%w: lifecycle has no requested event", ErrIllegalExecutionTransition)
	}
	if terminal {
		return fmt.Errorf("%w: %s cannot follow a terminal event", ErrIllegalExecutionTransition, candidate.Type)
	}

	switch candidate.Type {
	case execution.EventRequested:
		return fmt.Errorf("%w: requested event already exists", ErrIllegalExecutionTransition)
	case execution.EventApprovalRequested:
		if started || approved || approvalRequested || candidate.Approval != execution.ApprovalRequested {
			return fmt.Errorf("%w: approval_requested requires an unresolved pre-start request", ErrIllegalExecutionTransition)
		}
	case execution.EventApproved:
		validApproval := approvalGrant(candidate.Approval) ||
			(candidate.Approval == execution.ApprovalNotApplicable && candidate.Identity.EffectClass == execution.EffectReadOnly)
		if started || approved || !validApproval {
			return fmt.Errorf("%w: approved requires one valid pre-start approval source", ErrIllegalExecutionTransition)
		}
		if candidate.Approval == execution.ApprovalOnce && !approvalRequested {
			return fmt.Errorf("%w: interactive approval requires approval_requested", ErrIllegalExecutionTransition)
		}
	case execution.EventDenied:
		if started || candidate.Approval != execution.ApprovalUserDenied {
			return fmt.Errorf("%w: denied requires a pre-start denied decision", ErrIllegalExecutionTransition)
		}
	case execution.EventStarted:
		if started || (approvalRequested && !approved) {
			return fmt.Errorf("%w: started requires any pending approval to resolve", ErrIllegalExecutionTransition)
		}
		if candidate.Identity.EffectClass != execution.EffectReadOnly && !approved {
			return fmt.Errorf("%w: effectful or unknown execution requires an approved event", ErrIllegalExecutionTransition)
		}
	case execution.EventCompleted:
		if !started {
			return fmt.Errorf("%w: completed requires started", ErrIllegalExecutionTransition)
		}
	case execution.EventFailed:
		// An answered backend error is a verifiable terminal outcome for every
		// effect class. The agent reserves outcome_unknown for dispatches whose
		// receipt did not come from the effect owner (transport loss, host-side
		// kill, gateway relay without a typed domain receipt).
	case execution.EventCancelled:
		if started && candidate.Identity.EffectClass != execution.EffectReadOnly {
			return fmt.Errorf("%w: a started non-read-only execution must use outcome_unknown", ErrIllegalExecutionTransition)
		}
	case execution.EventOutcomeUnknown:
		if !started || candidate.Identity.EffectClass == execution.EffectReadOnly {
			return fmt.Errorf("%w: outcome_unknown requires a started effectful or unknown execution", ErrIllegalExecutionTransition)
		}
	default:
		return fmt.Errorf("%w: unsupported event %s", ErrIllegalExecutionTransition, candidate.Type)
	}
	return nil
}

func approvalGrant(approval execution.Approval) bool {
	switch approval {
	case execution.ApprovalPolicy, execution.ApprovalYolo, execution.ApprovalEmbedding,
		execution.ApprovalOnce, execution.ApprovalSession:
		return true
	default:
		return false
	}
}

func executionEventsEquivalent(stored, candidate execution.Event) bool {
	if stored.Identity != candidate.Identity ||
		stored.Type != candidate.Type ||
		stored.Approval != candidate.Approval ||
		stored.ArgumentsSHA256 != candidate.ArgumentsSHA256 ||
		stored.ResultSHA256 != candidate.ResultSHA256 ||
		stored.ResultReceipt != candidate.ResultReceipt ||
		stored.Detail != candidate.Detail {
		return false
	}
	return candidate.OccurredAt.IsZero() || stored.OccurredAt.Equal(candidate.OccurredAt)
}

func getExecutionEventByID(ctx context.Context, tx *sql.Tx, id int64) (execution.Event, error) {
	event, err := scanExecutionEvent(tx.QueryRowContext(ctx,
		`SELECT `+executionEventColumns+` FROM execution_events WHERE id = ?`, id))
	if err != nil {
		return execution.Event{}, fmt.Errorf("read appended execution event: %w", err)
	}
	return event, nil
}

func scanExecutionEvent(scanner executionRowScanner) (execution.Event, error) {
	var event execution.Event
	var kind, effectClass, eventType, approval string
	var occurredAt, recordedAt string
	err := scanner.Scan(
		&event.ID,
		&event.Identity.SessionID,
		&event.Identity.WorkspaceID,
		&event.Identity.RunID,
		&event.Identity.TurnID,
		&event.Identity.ExecutionID,
		&event.Identity.IdempotencyKey,
		&event.Identity.ProviderCallID,
		&event.Identity.CanonicalCallID,
		&event.Identity.Iteration,
		&event.Identity.Ordinal,
		&event.Identity.ToolName,
		&kind,
		&effectClass,
		&eventType,
		&approval,
		&event.ArgumentsSHA256,
		&event.ResultSHA256,
		&event.ResultReceipt,
		&event.Detail,
		&occurredAt,
		&recordedAt,
	)
	if err != nil {
		return execution.Event{}, fmt.Errorf("scan execution event: %w", err)
	}
	event.Identity.Kind = execution.Kind(kind)
	event.Identity.EffectClass = execution.EffectClass(effectClass)
	event.Type = execution.EventType(eventType)
	event.Approval = execution.Approval(approval)
	event.OccurredAt, err = parseExecutionTime(occurredAt)
	if err != nil {
		return execution.Event{}, fmt.Errorf("parse execution occurrence time: %w", err)
	}
	event.RecordedAt, err = parseExecutionTime(recordedAt)
	if err != nil {
		return execution.Event{}, fmt.Errorf("parse execution record time: %w", err)
	}
	return event, nil
}

func scanExecutionState(scanner executionRowScanner) (execution.Event, int, error) {
	var event execution.Event
	var kind, effectClass, eventType, approval string
	var occurredAt, recordedAt string
	var count int
	err := scanner.Scan(
		&event.ID,
		&event.Identity.SessionID,
		&event.Identity.WorkspaceID,
		&event.Identity.RunID,
		&event.Identity.TurnID,
		&event.Identity.ExecutionID,
		&event.Identity.IdempotencyKey,
		&event.Identity.ProviderCallID,
		&event.Identity.CanonicalCallID,
		&event.Identity.Iteration,
		&event.Identity.Ordinal,
		&event.Identity.ToolName,
		&kind,
		&effectClass,
		&eventType,
		&approval,
		&event.ArgumentsSHA256,
		&event.ResultSHA256,
		&event.ResultReceipt,
		&event.Detail,
		&occurredAt,
		&recordedAt,
		&count,
	)
	if err != nil {
		return execution.Event{}, 0, fmt.Errorf("scan execution state: %w", err)
	}
	event.Identity.Kind = execution.Kind(kind)
	event.Identity.EffectClass = execution.EffectClass(effectClass)
	event.Type = execution.EventType(eventType)
	event.Approval = execution.Approval(approval)
	event.OccurredAt, err = parseExecutionTime(occurredAt)
	if err != nil {
		return execution.Event{}, 0, fmt.Errorf("parse execution occurrence time: %w", err)
	}
	event.RecordedAt, err = parseExecutionTime(recordedAt)
	if err != nil {
		return execution.Event{}, 0, fmt.Errorf("parse execution record time: %w", err)
	}
	return event, count, nil
}

func validateExecutionListLimit(limit, maximum int) error {
	if limit <= 0 || limit > maximum {
		return fmt.Errorf("execution list limit must be between 1 and %d", maximum)
	}
	return nil
}

func formatExecutionTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func parseExecutionTime(value string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, value)
}
