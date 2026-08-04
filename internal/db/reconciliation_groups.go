package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/abdul-hamid-achik/sonar/internal/controlplane"
	"github.com/abdul-hamid-achik/sonar/internal/execution"
	"github.com/abdul-hamid-achik/sonar/internal/goal"
	"github.com/abdul-hamid-achik/sonar/internal/reconciliation"
)

var (
	ErrReconciliationGroupNotFound      = errors.New("reconciliation group not found")
	ErrReconciliationGroupConflict      = errors.New("reconciliation group conflicts with durable state")
	ErrReconciliationGroupIncomplete    = errors.New("reconciliation group still has unresolved execution members")
	ErrReconciliationProjectionRequired = errors.New("completed effect requires session projection repair")
	ErrReconciliationStaleEvidence      = errors.New("reconciliation evidence target is stale")
	ErrReconciliationRepairRequired     = errors.New("reconciliation durable state is mixed or corrupt")
)

const reconciliationGroupMaxMembers = reconciliation.MaxGroupMembers

const reconciliationGroupColumns = `
	id, group_item_id, idempotency_key, session_id, workspace_id,
	goal_id, turn_id, blocker_reference, snapshot_cursor,
	goal_snapshot_sha256, member_set_sha256, execution_member_count,
	payload_json, payload_sha256, created_at, recorded_at`

const reconciliationGroupMemberColumns = `
	id, group_item_id, control_item_id, execution_id, turn_id,
	event_id, event_type, effect_class, event_sha256,
	item_payload_sha256, recorded_at`

const reconciliationGroupResolutionColumns = `
	id, resolution_id, idempotency_key, group_item_id,
	session_id, workspace_id, evidence_json, evidence_sha256,
	resolved_by, resolved_at, recorded_at`

type EnsureReconciliationGroupRequest struct {
	SessionID               int64
	WorkspaceID             string
	ExpectedSessionRevision int64
}

type ReconciliationGroup struct {
	ID                   int64
	GroupItemID          string
	IdempotencyKey       string
	SessionID            int64
	WorkspaceID          string
	GoalID               string
	TurnID               string
	BlockerReference     string
	SnapshotCursor       int64
	GoalSnapshotSHA256   string
	MemberSetSHA256      string
	ExecutionMemberCount int
	PayloadJSON          string
	PayloadSHA256        string
	CreatedAt            time.Time
	RecordedAt           time.Time
	Members              []ReconciliationGroupMember
	ParentResolution     *ReconciliationGroupResolution
}

type ReconciliationGroupMember struct {
	ID                int64
	GroupItemID       string
	ControlItemID     string
	ExecutionID       string
	TurnID            string
	EventID           int64
	EventType         execution.EventType
	EffectClass       execution.EffectClass
	EventSHA256       string
	ItemPayloadSHA256 string
	RecordedAt        time.Time
	Resolved          bool
}

type ReconciliationGroupResolution struct {
	ID             int64
	ResolutionID   string
	IdempotencyKey string
	GroupItemID    string
	SessionID      int64
	WorkspaceID    string
	EvidenceJSON   string
	EvidenceSHA256 string
	ResolvedBy     string
	ResolvedAt     time.Time
	RecordedAt     time.Time
}

// EnsureReconciliationGroup creates or exactly replays the durable turn-level
// authority and its complete execution-member set. It never resolves evidence,
// changes session state, or schedules provider work.
func (s *Store) EnsureReconciliationGroup(ctx context.Context, lease *ExecutionSessionLease, request EnsureReconciliationGroupRequest) (ReconciliationGroup, bool, error) {
	if request.ExpectedSessionRevision < 0 {
		return ReconciliationGroup{}, false, fmt.Errorf("invalid expected session revision %d", request.ExpectedSessionRevision)
	}
	release, err := s.holdControlLease(ctx, lease, request.SessionID, request.WorkspaceID)
	if err != nil {
		return ReconciliationGroup{}, false, err
	}
	defer release()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ReconciliationGroup{}, false, fmt.Errorf("begin reconciliation group ensure: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	record, err := getSessionStateRecord(ctx, tx, request.SessionID)
	if err != nil {
		return ReconciliationGroup{}, false, err
	}
	if record.Revision != request.ExpectedSessionRevision {
		return ReconciliationGroup{}, false, fmt.Errorf("%w: durable session revision is %d, expected %d", ErrSessionStateConflict, record.Revision, request.ExpectedSessionRevision)
	}
	session, err := decodeReconciliationSession(record)
	if err != nil {
		return ReconciliationGroup{}, false, err
	}
	turnID, err := outcomeUnknownTurn(session.goal)
	if err != nil {
		return ReconciliationGroup{}, false, err
	}
	if existing, err := getReconciliationGroupByScopeTx(ctx, tx, request.SessionID, request.WorkspaceID, session.goal.ID, turnID); err == nil {
		if err := validateGroupAgainstBlockedSession(existing, session); err != nil {
			return ReconciliationGroup{}, false, err
		}
		if existing.ParentResolution != nil {
			return ReconciliationGroup{}, false, fmt.Errorf("%w: parent resolution exists while the owning goal remains blocked", ErrReconciliationRepairRequired)
		}
		if err := rejectCompletedPostCursorTx(ctx, tx, request.SessionID, request.WorkspaceID, existing.SnapshotCursor); err != nil {
			return ReconciliationGroup{}, false, err
		}
		if err := verifyReconciliationGroupMembersTx(ctx, tx, &existing, false); err != nil {
			return ReconciliationGroup{}, false, err
		}
		if err := tx.Commit(); err != nil {
			return ReconciliationGroup{}, false, fmt.Errorf("commit reconciliation group replay: %w", err)
		}
		return existing, false, nil
	} else if !errors.Is(err, ErrReconciliationGroupNotFound) {
		return ReconciliationGroup{}, false, err
	}
	if err := rejectCompletedPostCursorTx(ctx, tx, request.SessionID, request.WorkspaceID, session.executionCursor); err != nil {
		return ReconciliationGroup{}, false, err
	}
	hazards, err := listRawTurnReconciliationHazardsTx(ctx, tx, request.SessionID, request.WorkspaceID, turnID)
	if err != nil {
		return ReconciliationGroup{}, false, err
	}
	members := make([]ReconciliationGroupMember, 0, len(hazards))
	now := time.Now().UTC()
	for _, state := range hazards {
		item, err := ensureExecutionReconciliationItemTx(ctx, tx, session.goal, state, now)
		if err != nil {
			return ReconciliationGroup{}, false, err
		}
		eventSHA, err := executionEventDigest(state.Latest)
		if err != nil {
			return ReconciliationGroup{}, false, err
		}
		members = append(members, ReconciliationGroupMember{
			ControlItemID: item.ItemID, ExecutionID: state.Identity.ExecutionID,
			TurnID: state.Identity.TurnID, EventID: state.Latest.ID,
			EventType: state.Latest.Type, EffectClass: state.Identity.EffectClass,
			EventSHA256: eventSHA, ItemPayloadSHA256: item.PayloadSHA256,
		})
	}
	sort.Slice(members, func(i, j int) bool { return members[i].ExecutionID < members[j].ExecutionID })
	memberSetSHA, err := reconciliationMemberSetSHA(members)
	if err != nil {
		return ReconciliationGroup{}, false, err
	}
	identitySHA := controlplane.HashText(fmt.Sprintf("reconciliation-group\x00%d\x00%s\x00%s", request.SessionID, session.goal.ID, turnID))
	groupItemID := "recongrp_" + identitySHA[:32]
	payloadJSON, payloadSHA, err := controlplane.MarshalDocument(map[string]any{
		"schema":     "sonar.reconciliation-group/v1",
		"session_id": request.SessionID, "workspace_id": request.WorkspaceID,
		"goal_id": session.goal.ID, "turn_id": turnID,
		"blocker_reference":      session.goal.Blocker.Reference,
		"snapshot_cursor":        session.executionCursor,
		"goal_snapshot_sha256":   session.goalSHA256,
		"member_set_sha256":      memberSetSHA,
		"execution_member_count": len(members),
	})
	if err != nil {
		return ReconciliationGroup{}, false, err
	}
	group := ReconciliationGroup{
		GroupItemID: groupItemID, IdempotencyKey: "recongrp_idem_" + identitySHA[:32],
		SessionID: request.SessionID, WorkspaceID: request.WorkspaceID,
		GoalID: session.goal.ID, TurnID: turnID, BlockerReference: session.goal.Blocker.Reference,
		SnapshotCursor: session.executionCursor, GoalSnapshotSHA256: session.goalSHA256,
		MemberSetSHA256: memberSetSHA, ExecutionMemberCount: len(members),
		PayloadJSON: payloadJSON, PayloadSHA256: payloadSHA, CreatedAt: now,
	}
	stored, err := insertReconciliationGroupTx(ctx, tx, group, members)
	if err != nil {
		return ReconciliationGroup{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return ReconciliationGroup{}, false, fmt.Errorf("commit reconciliation group: %w", err)
	}
	return stored, true, nil
}

func ensureExecutionReconciliationItemTx(ctx context.Context, tx *sql.Tx, snapshot goal.Snapshot, state execution.State, now time.Time) (controlplane.Item, error) {
	identitySHA := controlplane.HashText(fmt.Sprintf("execution-reconciliation\x00%d\x00%s\x00%s", snapshot.SessionID, snapshot.ID, state.Identity.ExecutionID))
	payload, payloadSHA, err := controlplane.MarshalDocument(map[string]any{
		"execution_id": state.Identity.ExecutionID, "turn_id": state.Identity.TurnID,
		"tool": state.Identity.ToolName, "event_type": state.Latest.Type,
		"effect_class": state.Identity.EffectClass,
	})
	if err != nil {
		return controlplane.Item{}, err
	}
	expected := controlplane.Item{
		ItemID:         "ctrl_execution_" + identitySHA[:32],
		IdempotencyKey: "ctrlidem_execution_" + identitySHA[:32],
		Kind:           controlplane.KindExecutionReconciliation,
		Identity: controlplane.Identity{
			SessionID: snapshot.SessionID, WorkspaceID: state.Identity.WorkspaceID,
			GoalID: snapshot.ID, ExecutionID: state.Identity.ExecutionID, TurnID: state.Identity.TurnID,
		},
		ExternalID:  state.Identity.CanonicalCallID,
		Summary:     "Reconcile outcome of " + state.Identity.ToolName,
		PayloadJSON: payload, PayloadSHA256: payloadSHA,
	}

	rows, err := tx.QueryContext(ctx, `SELECT `+controlItemColumns+`
		FROM control_items
		WHERE session_id = ? AND workspace_id = ? AND execution_id = ?
		  AND kind = 'execution_reconciliation'
		ORDER BY id LIMIT 2`, state.Identity.SessionID, state.Identity.WorkspaceID, state.Identity.ExecutionID)
	if err != nil {
		return controlplane.Item{}, fmt.Errorf("query reconciliation member item: %w", err)
	}
	existing := make([]controlplane.Item, 0, 2)
	for rows.Next() {
		item, scanErr := scanControlItem(rows)
		if scanErr != nil {
			_ = rows.Close()
			return controlplane.Item{}, scanErr
		}
		existing = append(existing, item)
	}
	if err := rows.Close(); err != nil {
		return controlplane.Item{}, err
	}
	if len(existing) > 1 {
		return controlplane.Item{}, fmt.Errorf("%w: execution %q has duplicate control items", ErrReconciliationRepairRequired, state.Identity.ExecutionID)
	}
	if len(existing) == 1 {
		item := existing[0]
		// CreatedAt is repository-owned and deliberately absent from expected;
		// every caller-controlled immutable field must otherwise be canonical for
		// this exact current execution event. This rejects a generic control-plane
		// caller that pre-seeded a misleading or stale authority item.
		if err := item.Validate(); err != nil || !controlItemsEquivalent(item, expected) {
			return controlplane.Item{}, fmt.Errorf("%w: execution %q control item is not exact", ErrReconciliationRepairRequired, state.Identity.ExecutionID)
		}
		return item, nil
	}
	expected.CreatedAt = now
	stored, _, err := appendControlItemTx(ctx, tx, expected)
	return stored, err
}

func listRawTurnReconciliationHazardsTx(ctx context.Context, tx *sql.Tx, sessionID int64, workspaceID, turnID string) ([]execution.State, error) {
	rows, err := tx.QueryContext(ctx, `
		WITH ranked AS (
			SELECT e.*,
			       COUNT(*) OVER (PARTITION BY execution_id) AS event_count,
			       ROW_NUMBER() OVER (PARTITION BY execution_id ORDER BY id DESC) AS latest_rank
			  FROM execution_events e
			 WHERE session_id = ? AND workspace_id = ? AND turn_id = ?
		)
		SELECT `+executionEventColumns+`, event_count
		  FROM ranked
		 WHERE latest_rank = 1
		   AND (event_type = 'outcome_unknown' OR (event_type = 'started' AND effect_class != 'read_only'))
		 ORDER BY execution_id
		 LIMIT ?`, sessionID, workspaceID, turnID, reconciliationGroupMaxMembers+1)
	if err != nil {
		return nil, fmt.Errorf("query raw reconciliation turn hazards: %w", err)
	}
	defer func() { _ = rows.Close() }()
	states := make([]execution.State, 0)
	for rows.Next() {
		event, count, scanErr := scanExecutionState(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		states = append(states, execution.State{Identity: event.Identity, Latest: event, EventCount: count})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read raw reconciliation turn hazards: %w", err)
	}
	if len(states) > reconciliationGroupMaxMembers {
		return nil, fmt.Errorf("%w: turn has more than %d execution members", ErrExecutionHazardOverflow, reconciliationGroupMaxMembers)
	}
	return states, nil
}

func rejectCompletedPostCursorTx(ctx context.Context, tx *sql.Tx, sessionID int64, workspaceID string, cursor int64) error {
	var executionID, eventType string
	err := tx.QueryRowContext(ctx, `
		WITH ranked AS (
			SELECT e.*, ROW_NUMBER() OVER (PARTITION BY execution_id ORDER BY id DESC) AS latest_rank
			  FROM execution_events e
			 WHERE session_id = ? AND workspace_id = ?
		)
		SELECT execution_id, event_type FROM ranked
		 WHERE latest_rank = 1 AND event_type IN ('completed', 'failed')
		   AND effect_class != 'read_only' AND id > ?
		 ORDER BY id LIMIT 1`, sessionID, workspaceID, cursor).Scan(&executionID, &eventType)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect answered post-cursor effects: %w", err)
	}
	return fmt.Errorf("%w: execution %q %s after cursor %d", ErrReconciliationProjectionRequired, executionID, eventType, cursor)
}

type reconciliationMemberDigest struct {
	ControlItemID     string `json:"control_item_id"`
	ExecutionID       string `json:"execution_id"`
	EventID           int64  `json:"event_id"`
	EventType         string `json:"event_type"`
	EventSHA256       string `json:"event_sha256"`
	ItemPayloadSHA256 string `json:"item_payload_sha256"`
}

func reconciliationMemberSetSHA(members []ReconciliationGroupMember) (string, error) {
	values := make([]reconciliationMemberDigest, len(members))
	for index, member := range members {
		values[index] = reconciliationMemberDigest{
			ControlItemID: member.ControlItemID, ExecutionID: member.ExecutionID,
			EventID: member.EventID, EventType: string(member.EventType),
			EventSHA256: member.EventSHA256, ItemPayloadSHA256: member.ItemPayloadSHA256,
		}
	}
	sort.Slice(values, func(i, j int) bool { return values[i].ExecutionID < values[j].ExecutionID })
	encoded, err := json.Marshal(values)
	if err != nil {
		return "", fmt.Errorf("encode reconciliation member set: %w", err)
	}
	return reconciliation.Hash(string(encoded)), nil
}

func insertReconciliationGroupTx(ctx context.Context, tx *sql.Tx, group ReconciliationGroup, members []ReconciliationGroupMember) (ReconciliationGroup, error) {
	result, err := tx.ExecContext(ctx, `
		INSERT INTO reconciliation_groups (
			group_item_id, idempotency_key, session_id, workspace_id,
			goal_id, turn_id, blocker_reference, snapshot_cursor,
			goal_snapshot_sha256, member_set_sha256, execution_member_count,
			payload_json, payload_sha256, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		group.GroupItemID, group.IdempotencyKey, group.SessionID, group.WorkspaceID,
		group.GoalID, group.TurnID, group.BlockerReference, group.SnapshotCursor,
		group.GoalSnapshotSHA256, group.MemberSetSHA256, group.ExecutionMemberCount,
		group.PayloadJSON, group.PayloadSHA256, formatExecutionTime(group.CreatedAt))
	if err != nil {
		return ReconciliationGroup{}, fmt.Errorf("insert reconciliation group: %w", err)
	}
	groupID, err := result.LastInsertId()
	if err != nil {
		return ReconciliationGroup{}, err
	}
	for _, member := range members {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO reconciliation_group_members (
				group_item_id, control_item_id, execution_id, turn_id,
				event_id, event_type, effect_class, event_sha256, item_payload_sha256
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			group.GroupItemID, member.ControlItemID, member.ExecutionID, member.TurnID,
			member.EventID, member.EventType, member.EffectClass, member.EventSHA256, member.ItemPayloadSHA256); err != nil {
			return ReconciliationGroup{}, fmt.Errorf("insert reconciliation group member %q: %w", member.ExecutionID, err)
		}
	}
	return getReconciliationGroupByRowIDTx(ctx, tx, groupID)
}

func getReconciliationGroupByScopeTx(ctx context.Context, tx *sql.Tx, sessionID int64, workspaceID, goalID, turnID string) (ReconciliationGroup, error) {
	group, err := scanReconciliationGroup(tx.QueryRowContext(ctx, `SELECT `+reconciliationGroupColumns+`
		FROM reconciliation_groups
		WHERE session_id = ? AND workspace_id = ? AND goal_id = ? AND turn_id = ?`,
		sessionID, workspaceID, goalID, turnID))
	if errors.Is(err, sql.ErrNoRows) {
		return ReconciliationGroup{}, ErrReconciliationGroupNotFound
	}
	if err != nil {
		return ReconciliationGroup{}, fmt.Errorf("get reconciliation group by scope: %w", err)
	}
	return loadReconciliationGroupChildrenTx(ctx, tx, group)
}

func getReconciliationGroupByItemIDTx(ctx context.Context, tx *sql.Tx, groupItemID string) (ReconciliationGroup, error) {
	group, err := scanReconciliationGroup(tx.QueryRowContext(ctx, `SELECT `+reconciliationGroupColumns+`
		FROM reconciliation_groups WHERE group_item_id = ?`, groupItemID))
	if errors.Is(err, sql.ErrNoRows) {
		return ReconciliationGroup{}, ErrReconciliationGroupNotFound
	}
	if err != nil {
		return ReconciliationGroup{}, fmt.Errorf("get reconciliation group: %w", err)
	}
	return loadReconciliationGroupChildrenTx(ctx, tx, group)
}

func getReconciliationGroupByRowIDTx(ctx context.Context, tx *sql.Tx, id int64) (ReconciliationGroup, error) {
	group, err := scanReconciliationGroup(tx.QueryRowContext(ctx, `SELECT `+reconciliationGroupColumns+`
		FROM reconciliation_groups WHERE id = ?`, id))
	if err != nil {
		return ReconciliationGroup{}, fmt.Errorf("get inserted reconciliation group: %w", err)
	}
	return loadReconciliationGroupChildrenTx(ctx, tx, group)
}

func loadReconciliationGroupChildrenTx(ctx context.Context, tx *sql.Tx, group ReconciliationGroup) (ReconciliationGroup, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+reconciliationGroupMemberColumns+`
		FROM reconciliation_group_members WHERE group_item_id = ? ORDER BY execution_id`, group.GroupItemID)
	if err != nil {
		return ReconciliationGroup{}, err
	}
	for rows.Next() {
		member, scanErr := scanReconciliationGroupMember(rows)
		if scanErr != nil {
			_ = rows.Close()
			return ReconciliationGroup{}, scanErr
		}
		group.Members = append(group.Members, member)
	}
	if err := rows.Close(); err != nil {
		return ReconciliationGroup{}, err
	}
	if err := rows.Err(); err != nil {
		return ReconciliationGroup{}, err
	}
	for index := range group.Members {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM control_resolutions WHERE item_id = ?`, group.Members[index].ControlItemID).Scan(&count); err != nil {
			return ReconciliationGroup{}, err
		}
		if count > 1 {
			return ReconciliationGroup{}, fmt.Errorf("%w: member %q has duplicate resolutions", ErrReconciliationRepairRequired, group.Members[index].ExecutionID)
		}
		group.Members[index].Resolved = count == 1
	}
	resolution, err := scanReconciliationGroupResolution(tx.QueryRowContext(ctx, `SELECT `+reconciliationGroupResolutionColumns+`
		FROM reconciliation_group_resolutions WHERE group_item_id = ?`, group.GroupItemID))
	if err == nil {
		group.ParentResolution = &resolution
	} else if !errors.Is(err, sql.ErrNoRows) {
		return ReconciliationGroup{}, fmt.Errorf("read reconciliation group resolution: %w", err)
	}
	if err := validateReconciliationGroupRecord(group); err != nil {
		return ReconciliationGroup{}, err
	}
	return group, nil
}

type reconciliationGroupPayload struct {
	Schema               string `json:"schema"`
	SessionID            int64  `json:"session_id"`
	WorkspaceID          string `json:"workspace_id"`
	GoalID               string `json:"goal_id"`
	TurnID               string `json:"turn_id"`
	BlockerReference     string `json:"blocker_reference"`
	SnapshotCursor       int64  `json:"snapshot_cursor"`
	GoalSnapshotSHA256   string `json:"goal_snapshot_sha256"`
	MemberSetSHA256      string `json:"member_set_sha256"`
	ExecutionMemberCount int    `json:"execution_member_count"`
}

func validateReconciliationGroupRecord(group ReconciliationGroup) error {
	for _, value := range []string{group.GroupItemID, group.IdempotencyKey, group.WorkspaceID, group.GoalID, group.TurnID, group.BlockerReference} {
		if strings.TrimSpace(value) == "" || strings.TrimSpace(value) != value || !utf8.ValidString(value) {
			return fmt.Errorf("%w: group identity is not canonical", ErrReconciliationRepairRequired)
		}
	}
	if group.SessionID <= 0 || group.SnapshotCursor < 0 || group.ExecutionMemberCount != len(group.Members) ||
		group.ExecutionMemberCount > reconciliationGroupMaxMembers ||
		controlplane.HashText(group.PayloadJSON) != group.PayloadSHA256 {
		return fmt.Errorf("%w: group scalar or payload digest is invalid", ErrReconciliationRepairRequired)
	}
	identitySHA := controlplane.HashText(fmt.Sprintf("reconciliation-group\x00%d\x00%s\x00%s", group.SessionID, group.GoalID, group.TurnID))
	if group.GroupItemID != "recongrp_"+identitySHA[:32] || group.IdempotencyKey != "recongrp_idem_"+identitySHA[:32] {
		return fmt.Errorf("%w: group identity is not derived from its immutable scope", ErrReconciliationRepairRequired)
	}
	decoder := json.NewDecoder(strings.NewReader(group.PayloadJSON))
	decoder.DisallowUnknownFields()
	var payload reconciliationGroupPayload
	if err := decoder.Decode(&payload); err != nil {
		return fmt.Errorf("%w: decode group payload: %v", ErrReconciliationRepairRequired, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: group payload contains trailing data", ErrReconciliationRepairRequired)
	}
	if payload.Schema != "sonar.reconciliation-group/v1" || payload.SessionID != group.SessionID ||
		payload.WorkspaceID != group.WorkspaceID || payload.GoalID != group.GoalID || payload.TurnID != group.TurnID ||
		payload.BlockerReference != group.BlockerReference || payload.SnapshotCursor != group.SnapshotCursor ||
		payload.GoalSnapshotSHA256 != group.GoalSnapshotSHA256 || payload.MemberSetSHA256 != group.MemberSetSHA256 ||
		payload.ExecutionMemberCount != group.ExecutionMemberCount {
		return fmt.Errorf("%w: group payload does not match relational identity", ErrReconciliationRepairRequired)
	}
	canonicalPayload, canonicalSHA, err := controlplane.MarshalDocument(map[string]any{
		"schema": payload.Schema, "session_id": payload.SessionID, "workspace_id": payload.WorkspaceID,
		"goal_id": payload.GoalID, "turn_id": payload.TurnID, "blocker_reference": payload.BlockerReference,
		"snapshot_cursor": payload.SnapshotCursor, "goal_snapshot_sha256": payload.GoalSnapshotSHA256,
		"member_set_sha256": payload.MemberSetSHA256, "execution_member_count": payload.ExecutionMemberCount,
	})
	if err != nil || canonicalPayload != group.PayloadJSON || canonicalSHA != group.PayloadSHA256 {
		return fmt.Errorf("%w: group payload is not canonical: %v", ErrReconciliationRepairRequired, err)
	}
	memberSHA, err := reconciliationMemberSetSHA(group.Members)
	if err != nil || memberSHA != group.MemberSetSHA256 {
		return fmt.Errorf("%w: group member-set digest is invalid", ErrReconciliationRepairRequired)
	}
	if group.ParentResolution != nil {
		parent := *group.ParentResolution
		target := reconciliation.GroupTarget{
			SessionID: group.SessionID, WorkspaceID: group.WorkspaceID,
			GoalID: group.GoalID, TurnID: group.TurnID, GroupItemID: group.GroupItemID,
			GroupPayloadSHA256: group.PayloadSHA256, BlockerReference: group.BlockerReference,
			GoalSnapshotSHA256: group.GoalSnapshotSHA256, SnapshotCursor: group.SnapshotCursor,
			MemberSetSHA256: group.MemberSetSHA256, ExecutionMemberCount: group.ExecutionMemberCount,
			Actor: parent.ResolvedBy,
		}
		envelope, err := reconciliation.ParseGroup(parent.EvidenceJSON, parent.EvidenceSHA256)
		resolutionSHA := reconciliation.Hash("reconciliation-parent-resolution\x00" + group.GroupItemID + "\x00" + parent.EvidenceSHA256)
		if err != nil || !envelope.MatchesTarget(target) || parent.SessionID != group.SessionID ||
			parent.WorkspaceID != group.WorkspaceID || parent.GroupItemID != group.GroupItemID ||
			parent.ResolutionID != "recongrpres_"+resolutionSHA[:32] ||
			parent.IdempotencyKey != "recongrpres_idem_"+resolutionSHA[:32] {
			return fmt.Errorf("%w: parent resolution binding is invalid: %v", ErrReconciliationRepairRequired, err)
		}
	}
	return nil
}

type reconciliationRowScanner interface{ Scan(...any) error }

func scanReconciliationGroup(row reconciliationRowScanner) (ReconciliationGroup, error) {
	var group ReconciliationGroup
	var createdAt, recordedAt string
	err := row.Scan(&group.ID, &group.GroupItemID, &group.IdempotencyKey,
		&group.SessionID, &group.WorkspaceID, &group.GoalID, &group.TurnID,
		&group.BlockerReference, &group.SnapshotCursor, &group.GoalSnapshotSHA256,
		&group.MemberSetSHA256, &group.ExecutionMemberCount, &group.PayloadJSON,
		&group.PayloadSHA256, &createdAt, &recordedAt)
	if err != nil {
		return ReconciliationGroup{}, err
	}
	group.CreatedAt, err = parseExecutionTime(createdAt)
	if err != nil {
		return ReconciliationGroup{}, err
	}
	group.RecordedAt, err = parseExecutionTime(recordedAt)
	return group, err
}

func scanReconciliationGroupMember(row reconciliationRowScanner) (ReconciliationGroupMember, error) {
	var member ReconciliationGroupMember
	var eventType, effectClass, recordedAt string
	err := row.Scan(&member.ID, &member.GroupItemID, &member.ControlItemID,
		&member.ExecutionID, &member.TurnID, &member.EventID, &eventType,
		&effectClass, &member.EventSHA256, &member.ItemPayloadSHA256, &recordedAt)
	if err != nil {
		return ReconciliationGroupMember{}, err
	}
	member.EventType = execution.EventType(eventType)
	member.EffectClass = execution.EffectClass(effectClass)
	member.RecordedAt, err = parseExecutionTime(recordedAt)
	return member, err
}

func scanReconciliationGroupResolution(row reconciliationRowScanner) (ReconciliationGroupResolution, error) {
	var resolution ReconciliationGroupResolution
	var resolvedAt, recordedAt string
	err := row.Scan(&resolution.ID, &resolution.ResolutionID, &resolution.IdempotencyKey,
		&resolution.GroupItemID, &resolution.SessionID, &resolution.WorkspaceID,
		&resolution.EvidenceJSON, &resolution.EvidenceSHA256, &resolution.ResolvedBy,
		&resolvedAt, &recordedAt)
	if err != nil {
		return ReconciliationGroupResolution{}, err
	}
	resolution.ResolvedAt, err = parseExecutionTime(resolvedAt)
	if err != nil {
		return ReconciliationGroupResolution{}, err
	}
	resolution.RecordedAt, err = parseExecutionTime(recordedAt)
	return resolution, err
}

func validateGroupAgainstBlockedSession(group ReconciliationGroup, session reconciliationSession) error {
	turnID, err := outcomeUnknownTurn(session.goal)
	if err != nil {
		return err
	}
	if group.SessionID != session.record.SessionID || group.GoalID != session.goal.ID ||
		group.TurnID != turnID || group.BlockerReference != session.goal.Blocker.Reference ||
		group.SnapshotCursor != session.executionCursor || group.GoalSnapshotSHA256 != session.goalSHA256 {
		return fmt.Errorf("%w: group no longer matches the blocked session snapshot", ErrReconciliationStaleEvidence)
	}
	return nil
}

func verifyReconciliationGroupMembersTx(ctx context.Context, tx *sql.Tx, group *ReconciliationGroup, requireResolved bool) error {
	if group.ExecutionMemberCount != len(group.Members) || len(group.Members) > reconciliationGroupMaxMembers {
		return fmt.Errorf("%w: group member count mismatch", ErrReconciliationRepairRequired)
	}
	digest, err := reconciliationMemberSetSHA(group.Members)
	if err != nil || digest != group.MemberSetSHA256 {
		return fmt.Errorf("%w: group member-set digest mismatch", ErrReconciliationRepairRequired)
	}
	current, err := listRawTurnReconciliationHazardsTx(ctx, tx, group.SessionID, group.WorkspaceID, group.TurnID)
	if err != nil {
		return err
	}
	if len(current) != len(group.Members) {
		return fmt.Errorf("%w: current turn hazards differ from group membership", ErrReconciliationStaleEvidence)
	}
	byExecution := make(map[string]execution.State, len(current))
	for _, state := range current {
		byExecution[state.Identity.ExecutionID] = state
	}
	for index := range group.Members {
		member := &group.Members[index]
		state, exists := byExecution[member.ExecutionID]
		if !exists || state.Latest.ID != member.EventID || state.Latest.Type != member.EventType || state.Identity.EffectClass != member.EffectClass {
			return fmt.Errorf("%w: execution %q latest state changed", ErrReconciliationStaleEvidence, member.ExecutionID)
		}
		eventSHA, err := executionEventDigest(state.Latest)
		if err != nil || eventSHA != member.EventSHA256 {
			return fmt.Errorf("%w: execution %q event fingerprint changed", ErrReconciliationStaleEvidence, member.ExecutionID)
		}
		controlState, err := scanControlState(tx.QueryRowContext(ctx, controlStateSelect+`
			WHERE i.item_id = ? AND i.session_id = ? AND i.workspace_id = ?`,
			member.ControlItemID, group.SessionID, group.WorkspaceID))
		if err != nil || controlState.Item.Identity.GoalID != group.GoalID ||
			controlState.Item.Identity.TurnID != group.TurnID || controlState.Item.Identity.ExecutionID != member.ExecutionID ||
			controlState.Item.PayloadSHA256 != member.ItemPayloadSHA256 {
			return fmt.Errorf("%w: member %q control item changed", ErrReconciliationRepairRequired, member.ExecutionID)
		}
		member.Resolved = controlState.Resolution != nil
		if requireResolved && !member.Resolved {
			return ErrReconciliationGroupIncomplete
		}
		if member.Resolved {
			reconciled, err := executionStateEffectivelyReconciled(ctx, tx, state)
			if err != nil || !reconciled {
				return fmt.Errorf("%w: member %q resolution is not effective: %v", ErrReconciliationRepairRequired, member.ExecutionID, err)
			}
		}
	}
	return nil
}

func (s *Store) GetReconciliationGroup(ctx context.Context, sessionID int64, workspaceID, groupItemID string) (ReconciliationGroup, error) {
	if err := validateExecutionSessionScope(ctx, s.db, sessionID, workspaceID); err != nil {
		return ReconciliationGroup{}, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ReconciliationGroup{}, err
	}
	defer func() { _ = tx.Rollback() }()
	group, err := getReconciliationGroupByItemIDTx(ctx, tx, groupItemID)
	if err != nil {
		return ReconciliationGroup{}, err
	}
	if group.SessionID != sessionID || group.WorkspaceID != workspaceID {
		return ReconciliationGroup{}, ErrReconciliationGroupNotFound
	}
	return group, nil
}
