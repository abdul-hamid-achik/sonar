-- Checkpoints: immutable snapshots of a conversation's full message history.
-- They make compaction non-destructive (a pre-compaction snapshot is saved so
-- the original transcript can be restored) and enable "fork / try a different
-- approach" without losing state. session_id is a loose reference (no FK) so a
-- checkpoint can be taken mid-turn before a session row exists; 0 means unbound.
CREATE TABLE IF NOT EXISTS checkpoints (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id INTEGER NOT NULL DEFAULT 0,
    workspace_id TEXT  NOT NULL DEFAULT '',
    label      TEXT    NOT NULL DEFAULT '',
    kind       TEXT    NOT NULL DEFAULT 'manual', -- 'manual' | 'pre-compaction'
    messages   TEXT    NOT NULL DEFAULT '[]',     -- JSON-encoded []llm.Message
    msg_count  INTEGER NOT NULL DEFAULT 0,
    created_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE INDEX IF NOT EXISTS idx_checkpoints_session_id ON checkpoints(session_id);

-- Durable receipt for the explicit one-time adoption of pre-workspace,
-- pre-session checkpoints (workspace_id='', session_id=0). No foreign key is
-- intentional: deleting the destination session must not make the legacy set
-- claimable by another workspace later.
CREATE TABLE IF NOT EXISTS checkpoint_legacy_claims (
    claim_key     TEXT PRIMARY KEY,
    workspace_id  TEXT    NOT NULL,
    session_id    INTEGER NOT NULL,
    claimed_count INTEGER NOT NULL,
    created_at    TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
