package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Checkpoint kinds.
const (
	CheckpointManual        = "manual"
	CheckpointPreCompaction = "pre-compaction"
)

// ErrCheckpointNotFound is returned when a checkpoint id does not exist.
var ErrCheckpointNotFound = errors.New("checkpoint not found")

var ErrUnboundLegacyCheckpointsAlreadyClaimed = errors.New("unbound legacy checkpoints are already claimed by another workspace")

const unboundLegacyCheckpointClaimKey = "unbound-checkpoints-v1"

type LegacyCheckpointClaimReceipt struct {
	Claimed        int64
	AlreadyClaimed bool
	WorkspaceID    string
	SessionID      int64
}

// CreateCheckpoint stores a snapshot and returns its id. messagesJSON is stored
// verbatim; the caller owns its encoding (keeps this package free of llm types).
func (s *Store) CreateCheckpoint(ctx context.Context, sessionID int64, label, kind, messagesJSON string, msgCount int64) (int64, error) {
	return s.CreateCheckpointForWorkspace(ctx, sessionID, "", label, kind, messagesJSON, msgCount)
}

// CreateCheckpointForWorkspace stores a snapshot bound to a canonical
// workspace identity. New agent code uses this method; the legacy wrapper is
// retained only for compatibility with existing callers and migrations.
func (s *Store) CreateCheckpointForWorkspace(ctx context.Context, sessionID int64, workspaceID, label, kind, messagesJSON string, msgCount int64) (int64, error) {
	if kind == "" {
		kind = CheckpointManual
	}
	if messagesJSON == "" {
		messagesJSON = "[]"
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO checkpoints (session_id, workspace_id, label, kind, messages, msg_count) VALUES (?, ?, ?, ?, ?, ?)`,
		sessionID, workspaceID, label, kind, messagesJSON, msgCount,
	)
	if err != nil {
		return 0, fmt.Errorf("create checkpoint: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("checkpoint id: %w", err)
	}
	return id, nil
}

// ListCheckpoints returns checkpoints for a session, most recent first. The
// Messages field is omitted (left empty) to keep listings cheap; use
// GetCheckpoint to fetch the snapshot payload.
func (s *Store) ListCheckpoints(ctx context.Context, sessionID int64) ([]Checkpoint, error) {
	return s.listCheckpoints(ctx, sessionID, "", false)
}

func (s *Store) ListCheckpointsForWorkspace(ctx context.Context, sessionID int64, workspaceID string) ([]Checkpoint, error) {
	return s.listCheckpoints(ctx, sessionID, workspaceID, true)
}

func (s *Store) listCheckpoints(ctx context.Context, sessionID int64, workspaceID string, scoped bool) ([]Checkpoint, error) {
	query := `SELECT id, session_id, workspace_id, label, kind, msg_count, created_at
		   FROM checkpoints WHERE session_id = ? ORDER BY id DESC`
	args := []any{sessionID}
	if scoped {
		query = `SELECT id, session_id, workspace_id, label, kind, msg_count, created_at
		   FROM checkpoints WHERE session_id = ? AND workspace_id = ? ORDER BY id DESC`
		args = append(args, workspaceID)
	}
	rows, err := s.db.QueryContext(ctx,
		query, args...,
	)
	if err != nil {
		return nil, fmt.Errorf("list checkpoints: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Checkpoint
	for rows.Next() {
		var c Checkpoint
		if err := rows.Scan(&c.ID, &c.SessionID, &c.WorkspaceID, &c.Label, &c.Kind, &c.MsgCount, &c.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan checkpoint: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate checkpoints: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close checkpoint rows: %w", err)
	}
	return out, nil
}

// GetCheckpoint returns a single checkpoint including its messages payload.
func (s *Store) GetCheckpoint(ctx context.Context, id int64) (Checkpoint, error) {
	var c Checkpoint
	err := s.db.QueryRowContext(ctx,
		`SELECT id, session_id, workspace_id, label, kind, messages, msg_count, created_at
		   FROM checkpoints WHERE id = ?`,
		id,
	).Scan(&c.ID, &c.SessionID, &c.WorkspaceID, &c.Label, &c.Kind, &c.Messages, &c.MsgCount, &c.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Checkpoint{}, ErrCheckpointNotFound
	}
	if err != nil {
		return Checkpoint{}, fmt.Errorf("get checkpoint: %w", err)
	}
	return c, nil
}

// DeleteCheckpoint removes a checkpoint by id (no error if it does not exist).
func (s *Store) DeleteCheckpoint(ctx context.Context, id int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM checkpoints WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete checkpoint: %w", err)
	}
	return nil
}

// PruneCheckpoints keeps only the most recent keep checkpoints for a session,
// deleting older ones. A non-positive keep deletes none.
func (s *Store) PruneCheckpoints(ctx context.Context, sessionID int64, keep int) error {
	if keep <= 0 {
		return nil
	}
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM checkpoints
		  WHERE session_id = ?
		    AND id NOT IN (
		        SELECT id FROM checkpoints WHERE session_id = ? ORDER BY id DESC LIMIT ?
		    )`,
		sessionID, sessionID, keep,
	)
	if err != nil {
		return fmt.Errorf("prune checkpoints: %w", err)
	}
	return nil
}

// PruneCheckpointsByKind bounds automatic snapshots without deleting manual
// checkpoints the user intentionally created.
func (s *Store) PruneCheckpointsByKind(ctx context.Context, sessionID int64, kind string, keep int) error {
	return s.PruneCheckpointsByKindForWorkspace(ctx, sessionID, "", kind, keep)
}

func (s *Store) PruneCheckpointsByKindForWorkspace(ctx context.Context, sessionID int64, workspaceID, kind string, keep int) error {
	if keep <= 0 {
		return nil
	}
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM checkpoints
		  WHERE session_id = ? AND workspace_id = ? AND kind = ?
		    AND id NOT IN (
		        SELECT id FROM checkpoints
		         WHERE session_id = ? AND workspace_id = ? AND kind = ? ORDER BY id DESC LIMIT ?
		    )`,
		sessionID, workspaceID, kind, sessionID, workspaceID, kind, keep,
	)
	if err != nil {
		return fmt.Errorf("prune checkpoints by kind: %w", err)
	}
	return nil
}

// ClaimLegacyCheckpointsForActiveSession assigns pre-workspace checkpoints to
// the current workspace only after proving that the active session row is
// already bound to that exact workspace. Unbound session 0 and missing or
// differently scoped sessions are rejected; repeated calls are idempotent.
func (s *Store) ClaimLegacyCheckpointsForActiveSession(ctx context.Context, sessionID int64, workspaceID string) (int64, error) {
	if sessionID <= 0 {
		return 0, fmt.Errorf("an active persisted session is required to claim legacy checkpoints")
	}
	if workspaceID == "" {
		return 0, fmt.Errorf("workspace identity is required to claim legacy checkpoints")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin legacy checkpoint claim: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var sessionWorkspace string
	err = tx.QueryRowContext(ctx, `SELECT workspace_id FROM sessions WHERE id = ?`, sessionID).Scan(&sessionWorkspace)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("active session %d does not exist", sessionID)
	}
	if err != nil {
		return 0, fmt.Errorf("read active session workspace: %w", err)
	}
	if sessionWorkspace != workspaceID {
		return 0, fmt.Errorf("active session %d belongs to workspace %q, not %q", sessionID, sessionWorkspace, workspaceID)
	}

	result, err := tx.ExecContext(ctx,
		`UPDATE checkpoints SET workspace_id = ? WHERE session_id = ? AND workspace_id = ''`,
		workspaceID, sessionID,
	)
	if err != nil {
		return 0, fmt.Errorf("claim legacy checkpoints: %w", err)
	}
	claimed, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count claimed legacy checkpoints: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit legacy checkpoint claim: %w", err)
	}
	return claimed, nil
}

// CountUnboundLegacyCheckpoints previews the ambiguous pre-session set. The
// returned count must be shown to the user and supplied unchanged to the claim
// method, preventing a blind or stale one-time adoption.
func (s *Store) CountUnboundLegacyCheckpoints(ctx context.Context) (int64, error) {
	var count int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM checkpoints WHERE session_id = 0 AND workspace_id = ''`,
	).Scan(&count); err != nil {
		return 0, fmt.Errorf("count unbound legacy checkpoints: %w", err)
	}
	return count, nil
}

// ClaimUnboundLegacyCheckpointsForActiveSession explicitly rebinds the exact
// previewed set of legacy session_id=0 checkpoints to an already persisted,
// workspace-bound active session. A durable singleton receipt makes the claim
// globally one-time even if the destination session is later deleted.
func (s *Store) ClaimUnboundLegacyCheckpointsForActiveSession(ctx context.Context, activeSessionID int64, workspaceID string, expectedCount int64) (LegacyCheckpointClaimReceipt, error) {
	receipt := LegacyCheckpointClaimReceipt{WorkspaceID: workspaceID, SessionID: activeSessionID}
	if activeSessionID <= 0 {
		return receipt, fmt.Errorf("an active persisted session is required to claim unbound legacy checkpoints")
	}
	if workspaceID == "" {
		return receipt, fmt.Errorf("workspace identity is required to claim unbound legacy checkpoints")
	}
	if expectedCount < 0 {
		return receipt, fmt.Errorf("expected legacy checkpoint count must not be negative")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return receipt, fmt.Errorf("begin unbound legacy checkpoint claim: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var sessionWorkspace string
	err = tx.QueryRowContext(ctx, `SELECT workspace_id FROM sessions WHERE id = ?`, activeSessionID).Scan(&sessionWorkspace)
	if errors.Is(err, sql.ErrNoRows) {
		return receipt, fmt.Errorf("active session %d does not exist", activeSessionID)
	}
	if err != nil {
		return receipt, fmt.Errorf("read active session workspace: %w", err)
	}
	if sessionWorkspace != workspaceID {
		return receipt, fmt.Errorf("active session %d belongs to workspace %q, not %q", activeSessionID, sessionWorkspace, workspaceID)
	}

	var claimedWorkspace string
	var claimedSession, claimedCount int64
	err = tx.QueryRowContext(ctx,
		`SELECT workspace_id, session_id, claimed_count FROM checkpoint_legacy_claims WHERE claim_key = ?`,
		unboundLegacyCheckpointClaimKey,
	).Scan(&claimedWorkspace, &claimedSession, &claimedCount)
	if err == nil {
		if claimedWorkspace != workspaceID {
			return receipt, fmt.Errorf("%w %q", ErrUnboundLegacyCheckpointsAlreadyClaimed, claimedWorkspace)
		}
		var newUnbound int64
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM checkpoints WHERE session_id = 0 AND workspace_id = ''`,
		).Scan(&newUnbound); err != nil {
			return receipt, fmt.Errorf("count post-claim unbound checkpoints: %w", err)
		}
		if newUnbound != 0 {
			return receipt, fmt.Errorf("%d new unbound checkpoints appeared after the one-time claim", newUnbound)
		}
		receipt.AlreadyClaimed = true
		receipt.WorkspaceID = claimedWorkspace
		receipt.SessionID = claimedSession
		receipt.Claimed = claimedCount
		return receipt, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return receipt, fmt.Errorf("read unbound legacy checkpoint receipt: %w", err)
	}

	var actualCount int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM checkpoints WHERE session_id = 0 AND workspace_id = ''`,
	).Scan(&actualCount); err != nil {
		return receipt, fmt.Errorf("count claimable unbound legacy checkpoints: %w", err)
	}
	if actualCount != expectedCount {
		return receipt, fmt.Errorf("unbound legacy checkpoint count changed: previewed %d, now %d", expectedCount, actualCount)
	}
	if actualCount == 0 {
		return receipt, nil
	}

	result, err := tx.ExecContext(ctx,
		`UPDATE checkpoints SET session_id = ?, workspace_id = ? WHERE session_id = 0 AND workspace_id = ''`,
		activeSessionID, workspaceID,
	)
	if err != nil {
		return receipt, fmt.Errorf("rebind unbound legacy checkpoints: %w", err)
	}
	claimed, err := result.RowsAffected()
	if err != nil {
		return receipt, fmt.Errorf("count rebound legacy checkpoints: %w", err)
	}
	if claimed != actualCount {
		return receipt, fmt.Errorf("rebound %d legacy checkpoints, expected %d", claimed, actualCount)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO checkpoint_legacy_claims (claim_key, workspace_id, session_id, claimed_count) VALUES (?, ?, ?, ?)`,
		unboundLegacyCheckpointClaimKey, workspaceID, activeSessionID, claimed,
	); err != nil {
		return receipt, fmt.Errorf("record unbound legacy checkpoint receipt: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return receipt, fmt.Errorf("commit unbound legacy checkpoint claim: %w", err)
	}
	receipt.Claimed = claimed
	return receipt, nil
}
