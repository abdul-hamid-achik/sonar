package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// MaxSessionExportReadLimit is the largest page accepted by the bounded
// auxiliary-record readers used by session export.
const (
	MaxSessionExportReadLimit  = 1000
	MaxSessionExportStateBytes = 16 << 20
)

// ErrSessionExportStateTooLarge means the durable state cannot be included in
// a bounded audit projection without exceeding its explicit memory/output cap.
var ErrSessionExportStateTooLarge = errors.New("session state exceeds export byte limit")

// GetSessionStateForExport returns one complete durable state only when its
// UTF-8 byte representation fits maxBytes. The size check and read share one
// database snapshot, so growth cannot bypass the bound between queries.
func (s *Store) GetSessionStateForExport(ctx context.Context, sessionID int64, maxBytes int) (string, error) {
	if maxBytes <= 0 || maxBytes > MaxSessionExportStateBytes {
		return "", fmt.Errorf("session export state byte limit must be between 1 and %d", MaxSessionExportStateBytes)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin bounded session state read: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var size int64
	err = tx.QueryRowContext(ctx, `
		SELECT length(CAST(state_json AS BLOB))
		  FROM session_state
		 WHERE session_id = ?`, sessionID).Scan(&size)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrSessionStateNotFound
	}
	if err != nil {
		return "", fmt.Errorf("measure durable session state: %w", err)
	}
	if size > int64(maxBytes) {
		return "", fmt.Errorf("%w: session %d state is %d bytes (limit %d)", ErrSessionExportStateTooLarge, sessionID, size, maxBytes)
	}
	var state string
	if err := tx.QueryRowContext(ctx, `
		SELECT state_json
		  FROM session_state
		 WHERE session_id = ?`, sessionID).Scan(&state); err != nil {
		return "", fmt.Errorf("read bounded durable session state: %w", err)
	}
	if len(state) > maxBytes {
		return "", fmt.Errorf("%w: session %d state grew to %d bytes (limit %d)", ErrSessionExportStateTooLarge, sessionID, len(state), maxBytes)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit bounded session state read: %w", err)
	}
	return state, nil
}

// ListRecentSessionTokenStats returns the most recently recorded token stats
// for one session, ordered chronologically within the bounded result.
func (s *Store) ListRecentSessionTokenStats(ctx context.Context, sessionID int64, limit int) ([]TokenStat, error) {
	if err := validateSessionExportReadLimit(limit); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, session_id, turn, eval_count, prompt_tokens, model, created_at
		  FROM (
			SELECT id, session_id, turn, eval_count, prompt_tokens, model, created_at
			  FROM token_stats
			 WHERE session_id = ?
			 ORDER BY id DESC
			 LIMIT ?
		  )
		 ORDER BY id ASC`, sessionID, limit)
	if err != nil {
		return nil, fmt.Errorf("list recent session token stats: %w", err)
	}
	defer func() { _ = rows.Close() }()

	stats := make([]TokenStat, 0, limit)
	for rows.Next() {
		var stat TokenStat
		if err := rows.Scan(
			&stat.ID,
			&stat.SessionID,
			&stat.Turn,
			&stat.EvalCount,
			&stat.PromptTokens,
			&stat.Model,
			&stat.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan recent session token stat: %w", err)
		}
		stats = append(stats, stat)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read recent session token stats: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close recent session token stats: %w", err)
	}
	return stats, nil
}

// ListRecentSessionFileChanges returns the most recently recorded file changes
// for one session, ordered chronologically within the bounded result.
func (s *Store) ListRecentSessionFileChanges(ctx context.Context, sessionID int64, limit int) ([]FileChange, error) {
	if err := validateSessionExportReadLimit(limit); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, session_id, file_path, tool_name, added, removed, created_at
		  FROM (
			SELECT id, session_id, file_path, tool_name, added, removed, created_at
			  FROM file_changes
			 WHERE session_id = ?
			 ORDER BY id DESC
			 LIMIT ?
		  )
		 ORDER BY id ASC`, sessionID, limit)
	if err != nil {
		return nil, fmt.Errorf("list recent session file changes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	changes := make([]FileChange, 0, limit)
	for rows.Next() {
		var change FileChange
		if err := rows.Scan(
			&change.ID,
			&change.SessionID,
			&change.FilePath,
			&change.ToolName,
			&change.Added,
			&change.Removed,
			&change.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan recent session file change: %w", err)
		}
		changes = append(changes, change)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read recent session file changes: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close recent session file changes: %w", err)
	}
	return changes, nil
}

// ListRecentSessionCheckpoints returns the most recent checkpoint metadata for
// one session, newest first. Snapshot message bodies remain omitted.
func (s *Store) ListRecentSessionCheckpoints(ctx context.Context, sessionID int64, limit int) ([]Checkpoint, error) {
	if err := validateSessionExportReadLimit(limit); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, session_id, workspace_id, label, kind, msg_count, created_at
		  FROM checkpoints
		 WHERE session_id = ?
		 ORDER BY id DESC
		 LIMIT ?`, sessionID, limit)
	if err != nil {
		return nil, fmt.Errorf("list recent session checkpoints: %w", err)
	}
	defer func() { _ = rows.Close() }()

	checkpoints := make([]Checkpoint, 0, limit)
	for rows.Next() {
		var checkpoint Checkpoint
		if err := rows.Scan(
			&checkpoint.ID,
			&checkpoint.SessionID,
			&checkpoint.WorkspaceID,
			&checkpoint.Label,
			&checkpoint.Kind,
			&checkpoint.MsgCount,
			&checkpoint.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan recent session checkpoint: %w", err)
		}
		checkpoints = append(checkpoints, checkpoint)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read recent session checkpoints: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close recent session checkpoints: %w", err)
	}
	return checkpoints, nil
}

func validateSessionExportReadLimit(limit int) error {
	if limit <= 0 || limit > MaxSessionExportReadLimit {
		return fmt.Errorf("session export read limit must be between 1 and %d", MaxSessionExportReadLimit)
	}
	return nil
}
