package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/abdul-hamid-achik/sonar/internal/controlplane"
	"github.com/abdul-hamid-achik/sonar/internal/execution"
	"github.com/abdul-hamid-achik/sonar/internal/goal"
	"github.com/abdul-hamid-achik/sonar/internal/reconciliation"
)

type ResolveExecutionReconciliationRequest struct {
	SessionID               int64
	WorkspaceID             string
	GroupItemID             string
	ControlItemID           string
	ExpectedSessionRevision int64
	Actor                   string
	Evidence                reconciliation.Request
}

type ResolveReconciliationParentRequest struct {
	SessionID               int64
	WorkspaceID             string
	GroupItemID             string
	ExpectedSessionRevision int64
	Actor                   string
	Evidence                reconciliation.TurnRequest
}

type ReconciliationCommitReceipt struct {
	GroupItemID         string
	ItemID              string
	ResolutionID        string
	Inserted            bool
	GoalCleared         bool
	RemainingExecutions int
	ParentPending       bool
	SessionRevision     int64
	ExecutionCursor     int64
	Goal                *goal.Snapshot
}

// ResolveExecutionReconciliation appends one exact member receipt. It never
// clears the goal; the separately evidenced parent remains required and can be
// resolved only after every execution member is accounted for.
func (s *Store) ResolveExecutionReconciliation(ctx context.Context, lease *ExecutionSessionLease, request ResolveExecutionReconciliationRequest) (ReconciliationCommitReceipt, error) {
	if request.ExpectedSessionRevision < 0 {
		return ReconciliationCommitReceipt{}, fmt.Errorf("invalid expected session revision %d", request.ExpectedSessionRevision)
	}
	if err := request.Evidence.Validate(); err != nil {
		return ReconciliationCommitReceipt{}, err
	}
	release, err := s.holdControlLease(ctx, lease, request.SessionID, request.WorkspaceID)
	if err != nil {
		return ReconciliationCommitReceipt{}, err
	}
	defer release()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ReconciliationCommitReceipt{}, fmt.Errorf("begin execution reconciliation commit: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	group, err := getReconciliationGroupByItemIDTx(ctx, tx, request.GroupItemID)
	if err != nil {
		return ReconciliationCommitReceipt{}, err
	}
	if group.SessionID != request.SessionID || group.WorkspaceID != request.WorkspaceID {
		return ReconciliationCommitReceipt{}, ErrReconciliationGroupNotFound
	}
	member, found := groupMemberByItemID(group, request.ControlItemID)
	if !found {
		return ReconciliationCommitReceipt{}, fmt.Errorf("%w: item %q is not a member of group %q", ErrReconciliationGroupConflict, request.ControlItemID, request.GroupItemID)
	}
	state, err := getExecutionStateTx(ctx, tx, group.SessionID, group.WorkspaceID, member.ExecutionID)
	if err != nil {
		return ReconciliationCommitReceipt{}, err
	}
	if state.Latest.ID != member.EventID || state.Latest.Type != member.EventType || state.Identity.EffectClass != member.EffectClass {
		if (state.Latest.Type == execution.EventCompleted || state.Latest.Type == execution.EventFailed) &&
			state.Identity.EffectClass != execution.EffectReadOnly && state.Latest.ID > group.SnapshotCursor {
			return ReconciliationCommitReceipt{}, fmt.Errorf("%w: execution %q %s after group creation", ErrReconciliationProjectionRequired, member.ExecutionID, state.Latest.Type)
		}
		return ReconciliationCommitReceipt{}, fmt.Errorf("%w: execution %q changed after group creation", ErrReconciliationStaleEvidence, member.ExecutionID)
	}
	item, err := getControlItemByItemID(ctx, tx, member.ControlItemID)
	if err != nil {
		return ReconciliationCommitReceipt{}, err
	}
	target, err := executionReconciliationTarget(item, state.Latest, request.Actor)
	if err != nil {
		return ReconciliationCommitReceipt{}, fmt.Errorf("derive reconciliation member target: %w", err)
	}
	if target.LatestEventID != member.EventID || target.LatestEventSHA256 != member.EventSHA256 ||
		target.ItemPayloadSHA256 != member.ItemPayloadSHA256 {
		return ReconciliationCommitReceipt{}, fmt.Errorf("%w: execution %q changed after group creation", ErrReconciliationStaleEvidence, member.ExecutionID)
	}
	envelope, err := request.Evidence.Bind(target)
	if err != nil {
		return ReconciliationCommitReceipt{}, err
	}
	evidenceJSON, evidenceSHA, err := envelope.Marshal()
	if err != nil {
		return ReconciliationCommitReceipt{}, err
	}
	identitySHA := controlplane.HashText("reconciliation-member-resolution\x00" + group.GroupItemID + "\x00" + member.ControlItemID + "\x00" + evidenceSHA)
	candidate := controlplane.Resolution{
		ResolutionID:   "ctrlres_reconcile_" + identitySHA[:32],
		IdempotencyKey: "ctrlresidem_reconcile_" + identitySHA[:32],
		ItemID:         member.ControlItemID, SessionID: group.SessionID, WorkspaceID: group.WorkspaceID,
		Outcome: controlplane.OutcomeReconciled, EvidenceJSON: evidenceJSON, EvidenceSHA256: evidenceSHA,
		ResolvedBy: request.Actor, Detail: "operator supplied typed execution reconciliation evidence",
	}
	// Exact replay is checked before session revision so an ambiguous prior
	// commit remains observable even if the final parent later advanced it.
	if existing, exact, err := exactControlResolutionTx(ctx, tx, candidate); err != nil {
		return ReconciliationCommitReceipt{}, err
	} else if exact {
		remaining, err := countUnresolvedGroupMembersTx(ctx, tx, &group)
		if err != nil {
			return ReconciliationCommitReceipt{}, err
		}
		record, err := getSessionStateRecord(ctx, tx, group.SessionID)
		if err != nil {
			return ReconciliationCommitReceipt{}, err
		}
		return ReconciliationCommitReceipt{
			GroupItemID: group.GroupItemID, ItemID: member.ControlItemID,
			ResolutionID: existing.ResolutionID, Inserted: false,
			RemainingExecutions: remaining, ParentPending: group.ParentResolution == nil,
			SessionRevision: record.Revision,
		}, nil
	}
	if group.ParentResolution != nil {
		return ReconciliationCommitReceipt{}, fmt.Errorf("%w: unresolved execution member exists after the final parent resolution", ErrReconciliationRepairRequired)
	}
	record, err := getSessionStateRecord(ctx, tx, group.SessionID)
	if err != nil {
		return ReconciliationCommitReceipt{}, err
	}
	if record.Revision != request.ExpectedSessionRevision {
		return ReconciliationCommitReceipt{}, fmt.Errorf("%w: durable session revision is %d, expected %d", ErrSessionStateConflict, record.Revision, request.ExpectedSessionRevision)
	}
	session, err := decodeReconciliationSession(record)
	if err != nil {
		return ReconciliationCommitReceipt{}, err
	}
	if err := validateGroupAgainstBlockedSession(group, session); err != nil {
		return ReconciliationCommitReceipt{}, err
	}
	if err := rejectCompletedPostCursorTx(ctx, tx, group.SessionID, group.WorkspaceID, group.SnapshotCursor); err != nil {
		return ReconciliationCommitReceipt{}, err
	}
	if err := verifyReconciliationGroupMembersTx(ctx, tx, &group, false); err != nil {
		return ReconciliationCommitReceipt{}, err
	}
	stored, inserted, err := resolveControlItemTx(ctx, tx, candidate, true)
	if err != nil {
		return ReconciliationCommitReceipt{}, err
	}
	group, err = getReconciliationGroupByItemIDTx(ctx, tx, group.GroupItemID)
	if err != nil {
		return ReconciliationCommitReceipt{}, err
	}
	remaining, err := countUnresolvedGroupMembersTx(ctx, tx, &group)
	if err != nil {
		return ReconciliationCommitReceipt{}, err
	}
	if err := tx.Commit(); err != nil {
		readReceipt, readErr := s.readBackExecutionReconciliation(candidate, group.GroupItemID, request.ExpectedSessionRevision)
		if readErr == nil {
			return readReceipt, nil
		}
		return ReconciliationCommitReceipt{}, fmt.Errorf("commit execution reconciliation: %w (read-back: %v)", err, readErr)
	}
	return ReconciliationCommitReceipt{
		GroupItemID: group.GroupItemID, ItemID: member.ControlItemID,
		ResolutionID: stored.ResolutionID, Inserted: inserted,
		RemainingExecutions: remaining, ParentPending: true,
		SessionRevision: record.Revision,
	}, nil
}

// ResolveReconciliationParent appends the final turn-parent authority and the
// paused/exhausted Goal Runtime transition in one transaction. It never emits a
// provider, Resume, or supervisor command.
func (s *Store) ResolveReconciliationParent(ctx context.Context, lease *ExecutionSessionLease, request ResolveReconciliationParentRequest) (ReconciliationCommitReceipt, error) {
	if request.ExpectedSessionRevision < 0 {
		return ReconciliationCommitReceipt{}, fmt.Errorf("invalid expected session revision %d", request.ExpectedSessionRevision)
	}
	if err := request.Evidence.Validate(); err != nil {
		return ReconciliationCommitReceipt{}, err
	}
	release, err := s.holdControlLease(ctx, lease, request.SessionID, request.WorkspaceID)
	if err != nil {
		return ReconciliationCommitReceipt{}, err
	}
	defer release()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ReconciliationCommitReceipt{}, fmt.Errorf("begin reconciliation parent commit: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	group, err := getReconciliationGroupByItemIDTx(ctx, tx, request.GroupItemID)
	if err != nil {
		return ReconciliationCommitReceipt{}, err
	}
	if group.SessionID != request.SessionID || group.WorkspaceID != request.WorkspaceID {
		return ReconciliationCommitReceipt{}, ErrReconciliationGroupNotFound
	}
	target := reconciliation.GroupTarget{
		SessionID: group.SessionID, WorkspaceID: group.WorkspaceID,
		GoalID: group.GoalID, TurnID: group.TurnID, GroupItemID: group.GroupItemID,
		GroupPayloadSHA256: group.PayloadSHA256, BlockerReference: group.BlockerReference,
		GoalSnapshotSHA256: group.GoalSnapshotSHA256, SnapshotCursor: group.SnapshotCursor,
		MemberSetSHA256: group.MemberSetSHA256, ExecutionMemberCount: group.ExecutionMemberCount,
		Actor: request.Actor,
	}
	envelope, err := request.Evidence.Bind(target)
	if err != nil {
		return ReconciliationCommitReceipt{}, err
	}
	evidenceJSON, evidenceSHA, err := envelope.Marshal()
	if err != nil {
		return ReconciliationCommitReceipt{}, err
	}
	identitySHA := reconciliation.Hash("reconciliation-parent-resolution\x00" + group.GroupItemID + "\x00" + evidenceSHA)
	candidate := ReconciliationGroupResolution{
		ResolutionID:   "recongrpres_" + identitySHA[:32],
		IdempotencyKey: "recongrpres_idem_" + identitySHA[:32],
		GroupItemID:    group.GroupItemID, SessionID: group.SessionID, WorkspaceID: group.WorkspaceID,
		EvidenceJSON: evidenceJSON, EvidenceSHA256: evidenceSHA, ResolvedBy: request.Actor,
	}
	record, err := getSessionStateRecord(ctx, tx, group.SessionID)
	if err != nil {
		return ReconciliationCommitReceipt{}, err
	}
	session, err := decodeReconciliationSession(record)
	if err != nil {
		return ReconciliationCommitReceipt{}, err
	}
	if group.ParentResolution != nil {
		if !groupResolutionsEquivalent(*group.ParentResolution, candidate) {
			return ReconciliationCommitReceipt{}, fmt.Errorf("%w: parent evidence differs from immutable resolution", ErrReconciliationGroupConflict)
		}
		return verifiedFinalReplayReceipt(ctx, tx, group, session, record)
	}
	if record.Revision != request.ExpectedSessionRevision {
		return ReconciliationCommitReceipt{}, fmt.Errorf("%w: durable session revision is %d, expected %d", ErrSessionStateConflict, record.Revision, request.ExpectedSessionRevision)
	}
	if err := validateGroupAgainstBlockedSession(group, session); err != nil {
		return ReconciliationCommitReceipt{}, err
	}
	if err := rejectCompletedPostCursorTx(ctx, tx, group.SessionID, group.WorkspaceID, group.SnapshotCursor); err != nil {
		return ReconciliationCommitReceipt{}, err
	}
	if err := verifyReconciliationGroupMembersTx(ctx, tx, &group, true); err != nil {
		return ReconciliationCommitReceipt{}, err
	}
	if err := rejectOutsideGroupPostCursorHazardsTx(ctx, tx, group); err != nil {
		return ReconciliationCommitReceipt{}, err
	}
	storedParent, inserted, err := appendGroupResolutionTx(ctx, tx, group, candidate, target)
	if err != nil {
		return ReconciliationCommitReceipt{}, err
	}
	setSHA, targetCount, err := reconciliationResolutionSetSHATx(ctx, tx, group, storedParent)
	if err != nil {
		return ReconciliationCommitReceipt{}, err
	}
	goalResolution := goal.BlockResolution{
		Reference:  group.BlockerReference,
		Reason:     "reconciliation group inspected and fully accounted for",
		Reconciled: true, Evidence: request.Evidence.Summary,
		Reconciliation: &goal.ReconciliationReceipt{
			Version: goal.ReconciliationReceiptVersion, GroupItemID: group.GroupItemID,
			FinalItemID: group.GroupItemID, FinalResolutionID: storedParent.ResolutionID,
			ResolutionSetSHA256: setSHA, TargetCount: targetCount,
		},
	}
	nextGoal, err := goal.ApplyVerifiedReconciliation(ctx, session.goal, goalResolution, storedParent.ResolvedAt)
	if err != nil {
		return ReconciliationCommitReceipt{}, err
	}
	if nextGoal.State != goal.StatePaused && nextGoal.State != goal.StateExhausted {
		return ReconciliationCommitReceipt{}, fmt.Errorf("%w: verified reconciliation produced goal state %s", ErrReconciliationRepairRequired, nextGoal.State)
	}
	latestCursor, err := latestExecutionEventIDTx(ctx, tx, group.SessionID, group.WorkspaceID)
	if err != nil {
		return ReconciliationCommitReceipt{}, err
	}
	nextJSON, err := patchReconciliationSession(record.StateJSON, nextGoal, latestCursor)
	if err != nil {
		return ReconciliationCommitReceipt{}, err
	}
	nextRecord, err := saveSessionStateCASTx(ctx, tx, group.SessionID, request.ExpectedSessionRevision, nextJSON)
	if err != nil {
		return ReconciliationCommitReceipt{}, err
	}
	if err := touchSessionTx(ctx, tx, group.SessionID); err != nil {
		return ReconciliationCommitReceipt{}, err
	}
	if err := tx.Commit(); err != nil {
		readReceipt, readErr := s.readBackFinalReconciliation(candidate, nextRecord, goalResolution.Reconciliation)
		if readErr == nil {
			return readReceipt, nil
		}
		return ReconciliationCommitReceipt{}, fmt.Errorf("commit reconciliation parent: %w (read-back: %v)", err, readErr)
	}
	return ReconciliationCommitReceipt{
		GroupItemID: group.GroupItemID, ItemID: group.GroupItemID,
		ResolutionID: storedParent.ResolutionID, Inserted: inserted, GoalCleared: true,
		RemainingExecutions: 0, ParentPending: false,
		SessionRevision: nextRecord.Revision, ExecutionCursor: latestCursor, Goal: &nextGoal,
	}, nil
}

func groupMemberByItemID(group ReconciliationGroup, itemID string) (ReconciliationGroupMember, bool) {
	for _, member := range group.Members {
		if member.ControlItemID == itemID {
			return member, true
		}
	}
	return ReconciliationGroupMember{}, false
}

func getExecutionStateTx(ctx context.Context, tx *sql.Tx, sessionID int64, workspaceID, executionID string) (execution.State, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+executionEventColumns+`
		FROM execution_events WHERE session_id = ? AND workspace_id = ? AND execution_id = ? ORDER BY id`,
		sessionID, workspaceID, executionID)
	if err != nil {
		return execution.State{}, err
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
		return execution.State{}, err
	}
	if state.EventCount == 0 {
		return execution.State{}, ErrExecutionNotFound
	}
	return state, nil
}

func exactControlResolutionTx(ctx context.Context, tx *sql.Tx, candidate controlplane.Resolution) (controlplane.Resolution, bool, error) {
	existing, err := queryControlResolutionsByIdentity(ctx, tx, candidate.ResolutionID, candidate.IdempotencyKey, candidate.ItemID)
	if err != nil {
		return controlplane.Resolution{}, false, err
	}
	if len(existing) == 0 {
		return controlplane.Resolution{}, false, nil
	}
	if len(existing) == 1 && controlResolutionsEquivalent(existing[0], candidate) {
		return existing[0], true, nil
	}
	return controlplane.Resolution{}, false, ErrControlResolutionConflict
}

func countUnresolvedGroupMembersTx(ctx context.Context, tx *sql.Tx, group *ReconciliationGroup) (int, error) {
	if err := verifyReconciliationGroupMembersTx(ctx, tx, group, false); err != nil {
		return 0, err
	}
	remaining := 0
	for _, member := range group.Members {
		if !member.Resolved {
			remaining++
		}
	}
	return remaining, nil
}

func appendGroupResolutionTx(ctx context.Context, tx *sql.Tx, group ReconciliationGroup, candidate ReconciliationGroupResolution, target reconciliation.GroupTarget) (ReconciliationGroupResolution, bool, error) {
	if candidate.ResolvedAt.IsZero() {
		candidate.ResolvedAt = time.Now().UTC()
	} else {
		candidate.ResolvedAt = candidate.ResolvedAt.UTC()
	}
	if err := validateGroupResolution(candidate, group, target); err != nil {
		return ReconciliationGroupResolution{}, false, err
	}
	existing, err := queryGroupResolutionsByIdentityTx(ctx, tx, candidate)
	if err != nil {
		return ReconciliationGroupResolution{}, false, err
	}
	if len(existing) > 0 {
		if len(existing) == 1 && groupResolutionsEquivalent(existing[0], candidate) {
			return existing[0], false, nil
		}
		return ReconciliationGroupResolution{}, false, ErrReconciliationGroupConflict
	}
	result, err := tx.ExecContext(ctx, `
		INSERT INTO reconciliation_group_resolutions (
			resolution_id, idempotency_key, group_item_id, session_id, workspace_id,
			evidence_json, evidence_sha256, resolved_by, resolved_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		candidate.ResolutionID, candidate.IdempotencyKey, candidate.GroupItemID,
		candidate.SessionID, candidate.WorkspaceID, candidate.EvidenceJSON,
		candidate.EvidenceSHA256, candidate.ResolvedBy, formatExecutionTime(candidate.ResolvedAt))
	if err != nil {
		return ReconciliationGroupResolution{}, false, fmt.Errorf("insert reconciliation parent resolution: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return ReconciliationGroupResolution{}, false, err
	}
	stored, err := scanReconciliationGroupResolution(tx.QueryRowContext(ctx, `SELECT `+reconciliationGroupResolutionColumns+`
		FROM reconciliation_group_resolutions WHERE id = ?`, id))
	return stored, true, err
}

func validateGroupResolution(candidate ReconciliationGroupResolution, group ReconciliationGroup, target reconciliation.GroupTarget) error {
	if candidate.SessionID != group.SessionID || candidate.WorkspaceID != group.WorkspaceID || candidate.GroupItemID != group.GroupItemID {
		return ErrReconciliationGroupConflict
	}
	for _, value := range []string{candidate.ResolutionID, candidate.IdempotencyKey, candidate.GroupItemID, candidate.ResolvedBy} {
		if strings.TrimSpace(value) == "" || strings.TrimSpace(value) != value || !utf8.ValidString(value) {
			return ErrReconciliationGroupConflict
		}
	}
	identitySHA := reconciliation.Hash("reconciliation-parent-resolution\x00" + group.GroupItemID + "\x00" + candidate.EvidenceSHA256)
	if candidate.ResolutionID != "recongrpres_"+identitySHA[:32] || candidate.IdempotencyKey != "recongrpres_idem_"+identitySHA[:32] {
		return fmt.Errorf("%w: parent resolution identity is not derived from its evidence", ErrReconciliationGroupConflict)
	}
	envelope, err := reconciliation.ParseGroup(candidate.EvidenceJSON, candidate.EvidenceSHA256)
	if err != nil || !envelope.MatchesTarget(target) || envelope.Actor != candidate.ResolvedBy {
		return fmt.Errorf("%w: invalid parent evidence binding: %v", ErrReconciliationGroupConflict, err)
	}
	return nil
}

func queryGroupResolutionsByIdentityTx(ctx context.Context, tx *sql.Tx, candidate ReconciliationGroupResolution) ([]ReconciliationGroupResolution, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+reconciliationGroupResolutionColumns+`
		FROM reconciliation_group_resolutions
		WHERE resolution_id = ? OR idempotency_key = ? OR group_item_id = ? ORDER BY id`,
		candidate.ResolutionID, candidate.IdempotencyKey, candidate.GroupItemID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	values := make([]ReconciliationGroupResolution, 0, 3)
	for rows.Next() {
		value, scanErr := scanReconciliationGroupResolution(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

func groupResolutionsEquivalent(stored, candidate ReconciliationGroupResolution) bool {
	if stored.ResolutionID != candidate.ResolutionID || stored.IdempotencyKey != candidate.IdempotencyKey ||
		stored.GroupItemID != candidate.GroupItemID || stored.SessionID != candidate.SessionID ||
		stored.WorkspaceID != candidate.WorkspaceID || stored.EvidenceJSON != candidate.EvidenceJSON ||
		stored.EvidenceSHA256 != candidate.EvidenceSHA256 || stored.ResolvedBy != candidate.ResolvedBy {
		return false
	}
	return candidate.ResolvedAt.IsZero() || stored.ResolvedAt.Equal(candidate.ResolvedAt)
}

type resolutionSetEntry struct {
	Kind           string `json:"kind"`
	ItemID         string `json:"item_id"`
	ResolutionID   string `json:"resolution_id"`
	EvidenceSHA256 string `json:"evidence_sha256"`
}

func reconciliationResolutionSetSHATx(ctx context.Context, tx *sql.Tx, group ReconciliationGroup, parent ReconciliationGroupResolution) (string, int, error) {
	entries := []resolutionSetEntry{{
		Kind: "turn_parent", ItemID: group.GroupItemID,
		ResolutionID: parent.ResolutionID, EvidenceSHA256: parent.EvidenceSHA256,
	}}
	for _, member := range group.Members {
		resolution, err := scanControlResolution(tx.QueryRowContext(ctx, `SELECT `+controlResolutionColumns+`
			FROM control_resolutions WHERE item_id = ?`, member.ControlItemID))
		if err != nil {
			return "", 0, fmt.Errorf("read member resolution set: %w", err)
		}
		entries = append(entries, resolutionSetEntry{
			Kind: "execution", ItemID: member.ControlItemID,
			ResolutionID: resolution.ResolutionID, EvidenceSHA256: resolution.EvidenceSHA256,
		})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Kind == entries[j].Kind {
			return entries[i].ItemID < entries[j].ItemID
		}
		return entries[i].Kind < entries[j].Kind
	})
	encoded, err := json.Marshal(entries)
	if err != nil {
		return "", 0, err
	}
	return reconciliation.Hash(string(encoded)), len(entries), nil
}

func latestExecutionEventIDTx(ctx context.Context, tx *sql.Tx, sessionID int64, workspaceID string) (int64, error) {
	var latest int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM execution_events
		WHERE session_id = ? AND workspace_id = ?`, sessionID, workspaceID).Scan(&latest); err != nil {
		return 0, err
	}
	return latest, nil
}

func rejectOutsideGroupPostCursorHazardsTx(ctx context.Context, tx *sql.Tx, group ReconciliationGroup) error {
	members := make(map[string]struct{}, len(group.Members))
	for _, member := range group.Members {
		members[member.ExecutionID] = struct{}{}
	}
	rows, err := tx.QueryContext(ctx, `
		WITH ranked AS (
			SELECT e.*, COUNT(*) OVER (PARTITION BY execution_id) AS event_count,
			       ROW_NUMBER() OVER (PARTITION BY execution_id ORDER BY id DESC) AS latest_rank
			  FROM execution_events e WHERE session_id = ? AND workspace_id = ?
		)
		SELECT `+executionEventColumns+`, event_count FROM ranked
		 WHERE latest_rank = 1 AND id > ?
		   AND (event_type = 'outcome_unknown' OR (event_type = 'started' AND effect_class != 'read_only'))
		 ORDER BY id LIMIT ?`, group.SessionID, group.WorkspaceID, group.SnapshotCursor, reconciliationGroupMaxMembers+1)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	count := 0
	for rows.Next() {
		event, eventCount, scanErr := scanExecutionState(rows)
		if scanErr != nil {
			return scanErr
		}
		count++
		if _, inside := members[event.Identity.ExecutionID]; inside {
			continue
		}
		_ = eventCount
		return fmt.Errorf("%w: post-cursor execution %q is outside frozen group %q", ErrReconciliationStaleEvidence, event.Identity.ExecutionID, group.GroupItemID)
	}
	if count > reconciliationGroupMaxMembers {
		return ErrExecutionHazardOverflow
	}
	return rows.Err()
}

func verifiedFinalReplayReceipt(ctx context.Context, tx *sql.Tx, group ReconciliationGroup, session reconciliationSession, record SessionStateRecord) (ReconciliationCommitReceipt, error) {
	if group.ParentResolution == nil {
		return ReconciliationCommitReceipt{}, ErrReconciliationRepairRequired
	}
	if session.goal.ID != group.GoalID || session.goal.SessionID != group.SessionID {
		return ReconciliationCommitReceipt{}, fmt.Errorf("%w: final goal identity does not match reconciliation group", ErrReconciliationRepairRequired)
	}
	// A completed replay is still an authority check, not a blind idempotency
	// acknowledgement. Re-read the immutable members and all post-snapshot effect
	// boundaries so a copied receipt or later lifecycle change fails closed.
	if err := rejectCompletedPostCursorTx(ctx, tx, group.SessionID, group.WorkspaceID, group.SnapshotCursor); err != nil {
		return ReconciliationCommitReceipt{}, err
	}
	if err := verifyReconciliationGroupMembersTx(ctx, tx, &group, true); err != nil {
		return ReconciliationCommitReceipt{}, err
	}
	if err := rejectOutsideGroupPostCursorHazardsTx(ctx, tx, group); err != nil {
		return ReconciliationCommitReceipt{}, err
	}
	latestCursor, err := latestExecutionEventIDTx(ctx, tx, group.SessionID, group.WorkspaceID)
	if err != nil {
		return ReconciliationCommitReceipt{}, err
	}
	if session.executionCursor != latestCursor {
		return ReconciliationCommitReceipt{}, fmt.Errorf("%w: final execution cursor is %d, exact durable cursor is %d", ErrReconciliationRepairRequired, session.executionCursor, latestCursor)
	}
	parentEvidence, err := reconciliation.ParseGroup(group.ParentResolution.EvidenceJSON, group.ParentResolution.EvidenceSHA256)
	if err != nil {
		return ReconciliationCommitReceipt{}, fmt.Errorf("%w: final parent evidence is invalid: %v", ErrReconciliationRepairRequired, err)
	}
	setSHA, targetCount, err := reconciliationResolutionSetSHATx(ctx, tx, group, *group.ParentResolution)
	if err != nil {
		return ReconciliationCommitReceipt{}, err
	}
	expected := goal.ReconciliationReceipt{
		Version: goal.ReconciliationReceiptVersion, GroupItemID: group.GroupItemID,
		FinalItemID: group.GroupItemID, FinalResolutionID: group.ParentResolution.ResolutionID,
		ResolutionSetSHA256: setSHA, TargetCount: targetCount,
	}
	receipt := session.goal.LastBlockResolution
	if receipt == nil || receipt.Resolution.Reconciliation == nil ||
		*receipt.Resolution.Reconciliation != expected ||
		receipt.Kind != goal.BlockOutcomeUnknown || receipt.Reference != group.BlockerReference ||
		receipt.Resolution.Reference != group.BlockerReference || !receipt.Resolution.Reconciled ||
		receipt.Resolution.Reason != "reconciliation group inspected and fully accounted for" ||
		receipt.Resolution.Evidence != parentEvidence.Summary ||
		!receipt.ResolvedAt.Equal(group.ParentResolution.ResolvedAt) ||
		(session.goal.State != goal.StatePaused && session.goal.State != goal.StateExhausted) {
		return ReconciliationCommitReceipt{}, fmt.Errorf("%w: parent resolution and goal receipt are mixed", ErrReconciliationRepairRequired)
	}
	return ReconciliationCommitReceipt{
		GroupItemID: group.GroupItemID, ItemID: group.GroupItemID,
		ResolutionID: group.ParentResolution.ResolutionID, Inserted: false, GoalCleared: true,
		SessionRevision: record.Revision, ExecutionCursor: session.executionCursor, Goal: &session.goal,
	}, nil
}

func (s *Store) readBackExecutionReconciliation(candidate controlplane.Resolution, groupItemID string, expectedRevision int64) (ReconciliationCommitReceipt, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	state, err := s.GetControlState(ctx, candidate.SessionID, candidate.WorkspaceID, candidate.ItemID)
	if err != nil || state.Resolution == nil || !controlResolutionsEquivalent(*state.Resolution, candidate) {
		return ReconciliationCommitReceipt{}, fmt.Errorf("execution resolution read-back mismatch: %v", err)
	}
	group, err := s.GetReconciliationGroup(ctx, candidate.SessionID, candidate.WorkspaceID, groupItemID)
	if err != nil {
		return ReconciliationCommitReceipt{}, err
	}
	remaining := 0
	for _, member := range group.Members {
		if !member.Resolved {
			remaining++
		}
	}
	record, err := s.GetSessionStateRecord(ctx, candidate.SessionID)
	if err != nil || record.Revision != expectedRevision {
		return ReconciliationCommitReceipt{}, fmt.Errorf("partial resolution changed session revision: %v", err)
	}
	return ReconciliationCommitReceipt{
		GroupItemID: groupItemID, ItemID: candidate.ItemID,
		ResolutionID: state.Resolution.ResolutionID, RemainingExecutions: remaining,
		ParentPending: group.ParentResolution == nil, SessionRevision: record.Revision,
	}, nil
}

func (s *Store) readBackFinalReconciliation(candidate ReconciliationGroupResolution, expected SessionStateRecord, receipt *goal.ReconciliationReceipt) (ReconciliationCommitReceipt, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	group, err := s.GetReconciliationGroup(ctx, candidate.SessionID, candidate.WorkspaceID, candidate.GroupItemID)
	if err != nil || group.ParentResolution == nil || !groupResolutionsEquivalent(*group.ParentResolution, candidate) {
		return ReconciliationCommitReceipt{}, fmt.Errorf("parent resolution read-back mismatch: %v", err)
	}
	record, err := s.GetSessionStateRecord(ctx, candidate.SessionID)
	if err != nil || record.Revision != expected.Revision || record.StateJSON != expected.StateJSON {
		return ReconciliationCommitReceipt{}, fmt.Errorf("%w: final session read-back mismatch: %v", ErrReconciliationRepairRequired, err)
	}
	session, err := decodeReconciliationSession(record)
	if err != nil || session.goal.LastBlockResolution == nil || session.goal.LastBlockResolution.Resolution.Reconciliation == nil ||
		*session.goal.LastBlockResolution.Resolution.Reconciliation != *receipt {
		return ReconciliationCommitReceipt{}, fmt.Errorf("%w: final goal receipt read-back mismatch: %v", ErrReconciliationRepairRequired, err)
	}
	return ReconciliationCommitReceipt{
		GroupItemID: group.GroupItemID, ItemID: group.GroupItemID,
		ResolutionID: candidate.ResolutionID, GoalCleared: true,
		SessionRevision: record.Revision, ExecutionCursor: session.executionCursor, Goal: &session.goal,
	}, nil
}
