-- name: GetToolPermission :one
SELECT * FROM tool_permissions WHERE tool_name = ?;

-- name: UpsertToolPermission :one
INSERT INTO tool_permissions (tool_name, policy)
VALUES (?, ?)
ON CONFLICT(tool_name) DO UPDATE SET policy = excluded.policy, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
RETURNING *;

-- name: ListToolPermissions :many
SELECT * FROM tool_permissions ORDER BY tool_name ASC;

-- name: DeleteToolPermission :exec
DELETE FROM tool_permissions WHERE tool_name = ?;

-- name: ResetToolPermissions :exec
DELETE FROM tool_permissions;
