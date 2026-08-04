-- Lossless UI + agent state for durable completed-turn resume. The normalized
-- session_messages table remains available for future search/analytics; this
-- payload is the authoritative round-trip representation. In-flight effect
-- recovery requires the future append-only turn/tool event log.
CREATE TABLE IF NOT EXISTS session_state (
    session_id INTEGER PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
    state_json TEXT NOT NULL DEFAULT '{}',
	revision INTEGER NOT NULL DEFAULT 0 CHECK (revision >= 0),
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
