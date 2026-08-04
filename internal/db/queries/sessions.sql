-- name: CreateSession :one
INSERT INTO sessions (title, model, mode, workspace_id, public_id) VALUES (?, ?, ?, ?, ?) RETURNING *;

-- name: GetSession :one
SELECT * FROM sessions WHERE id = ?;

-- name: GetSessionByPublicID :one
SELECT * FROM sessions WHERE public_id = ?;

-- name: ListSessions :many
SELECT * FROM sessions WHERE workspace_id = ? ORDER BY updated_at DESC, id DESC LIMIT ?;

-- name: UpdateSessionTitle :exec
UPDATE sessions SET title = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE id = ?;

-- name: UpdateSessionTimestamp :exec
UPDATE sessions SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE id = ?;

-- name: DeleteSession :exec
DELETE FROM sessions WHERE id = ?;

-- name: CreateSessionMessage :one
INSERT INTO session_messages (session_id, role, content, tool_name, tool_args, is_error, thinking)
VALUES (?, ?, ?, ?, ?, ?, ?) RETURNING *;

-- name: GetSessionMessages :many
SELECT * FROM session_messages WHERE session_id = ? ORDER BY id ASC;

-- name: CountSessions :one
SELECT COUNT(*) FROM sessions;
