-- Sessions table: replaces noted CLI dependency for session persistence.
CREATE TABLE IF NOT EXISTS sessions (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    title      TEXT    NOT NULL DEFAULT '',
    model      TEXT    NOT NULL DEFAULT '',
    mode       TEXT    NOT NULL DEFAULT 'BUILD',
    workspace_id TEXT  NOT NULL DEFAULT '',
    created_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

-- Session messages: individual chat entries within a session.
CREATE TABLE IF NOT EXISTS session_messages (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id INTEGER NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    role       TEXT    NOT NULL, -- 'user', 'assistant', 'tool', 'system', 'error'
    content    TEXT    NOT NULL DEFAULT '',
    tool_name  TEXT    NOT NULL DEFAULT '',
    tool_args  TEXT    NOT NULL DEFAULT '',
    is_error   INTEGER NOT NULL DEFAULT 0,
    thinking   TEXT    NOT NULL DEFAULT '',
    created_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE INDEX IF NOT EXISTS idx_session_messages_session_id ON session_messages(session_id);

-- Tool permissions: per-tool allow/deny/always-allow.
CREATE TABLE IF NOT EXISTS tool_permissions (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    tool_name  TEXT    NOT NULL UNIQUE,
    policy     TEXT    NOT NULL DEFAULT 'ask', -- 'allow', 'deny', 'ask'
    updated_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

-- Token usage stats: per-turn tracking.
CREATE TABLE IF NOT EXISTS token_stats (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id    INTEGER NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    turn          INTEGER NOT NULL DEFAULT 0,
    eval_count    INTEGER NOT NULL DEFAULT 0,
    prompt_tokens INTEGER NOT NULL DEFAULT 0,
    model         TEXT    NOT NULL DEFAULT '',
    created_at    TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE INDEX IF NOT EXISTS idx_token_stats_session_id ON token_stats(session_id);

-- File changes: files modified by agent during a session.
CREATE TABLE IF NOT EXISTS file_changes (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id INTEGER NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    file_path  TEXT    NOT NULL,
    tool_name  TEXT    NOT NULL DEFAULT '',
    added      INTEGER NOT NULL DEFAULT 0,
    removed    INTEGER NOT NULL DEFAULT 0,
    created_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE INDEX IF NOT EXISTS idx_file_changes_session_id ON file_changes(session_id);
