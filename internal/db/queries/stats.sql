-- name: RecordTokenUsage :one
INSERT INTO token_stats (session_id, turn, eval_count, prompt_tokens, model)
VALUES (?, ?, ?, ?, ?) RETURNING *;

-- name: GetSessionTokenStats :many
SELECT * FROM token_stats WHERE session_id = ? ORDER BY turn ASC;

-- name: GetSessionTotalTokens :one
SELECT
    CAST(COALESCE(SUM(eval_count), 0) AS INTEGER) AS total_eval,
    CAST(COALESCE(SUM(prompt_tokens), 0) AS INTEGER) AS total_prompt,
    CAST(COUNT(*) AS INTEGER) AS turn_count
FROM token_stats WHERE session_id = ?;

-- name: RecordFileChange :one
INSERT INTO file_changes (session_id, file_path, tool_name, added, removed)
VALUES (?, ?, ?, ?, ?) RETURNING *;

-- name: GetSessionFileChanges :many
SELECT * FROM file_changes WHERE session_id = ? ORDER BY created_at ASC;

-- name: GetSessionFileChangeSummary :many
SELECT
    file_path,
    CAST(COALESCE(SUM(added), 0) AS INTEGER) AS total_added,
    CAST(COALESCE(SUM(removed), 0) AS INTEGER) AS total_removed,
    CAST(COUNT(*) AS INTEGER) AS change_count
FROM file_changes
WHERE session_id = ?
GROUP BY file_path
ORDER BY file_path ASC;
