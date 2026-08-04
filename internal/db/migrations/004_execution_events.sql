-- Immutable execution lifecycle ledger. session_state remains the authoritative
-- completed-turn projection; these rows preserve effect boundaries needed for
-- conservative crash recovery.
CREATE TABLE IF NOT EXISTS execution_events (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id        INTEGER NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    workspace_id      TEXT    NOT NULL,
    run_id            TEXT    NOT NULL,
    turn_id           TEXT    NOT NULL,
    execution_id      TEXT    NOT NULL,
    idempotency_key   TEXT    NOT NULL,
    provider_call_id  TEXT    NOT NULL DEFAULT '',
    canonical_call_id TEXT    NOT NULL,
    iteration         INTEGER NOT NULL,
    ordinal           INTEGER NOT NULL,
    tool_name         TEXT    NOT NULL,
    kind              TEXT    NOT NULL,
    effect_class      TEXT    NOT NULL,
    event_type        TEXT    NOT NULL,
    approval          TEXT    NOT NULL DEFAULT 'not_applicable',
    arguments_sha256  TEXT    NOT NULL,
    result_sha256     TEXT    NOT NULL DEFAULT '',
    result_receipt    TEXT    NOT NULL DEFAULT '',
    detail            TEXT    NOT NULL DEFAULT '',
    occurred_at       TEXT    NOT NULL,
    recorded_at       TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),

    CHECK (session_id > 0),
    CHECK (length(trim(workspace_id)) > 0 AND length(CAST(workspace_id AS BLOB)) <= 4096),
    CHECK (length(trim(run_id)) > 0 AND length(CAST(run_id AS BLOB)) <= 128),
    CHECK (length(trim(turn_id)) > 0 AND length(CAST(turn_id AS BLOB)) <= 128),
    CHECK (length(trim(execution_id)) > 0 AND length(CAST(execution_id AS BLOB)) <= 128),
    CHECK (length(trim(idempotency_key)) > 0 AND length(CAST(idempotency_key AS BLOB)) <= 128),
    CHECK (length(CAST(provider_call_id AS BLOB)) <= 1024),
    CHECK (length(trim(canonical_call_id)) > 0 AND length(CAST(canonical_call_id AS BLOB)) <= 1024),
    CHECK (length(trim(tool_name)) > 0 AND length(CAST(tool_name AS BLOB)) <= 512),
    CHECK (iteration > 0),
    CHECK (ordinal > 0),
    CHECK (kind IN ('builtin', 'memory', 'mcp')),
    CHECK (effect_class IN ('read_only', 'effectful', 'unknown')),
    CHECK (event_type IN (
        'requested', 'approval_requested', 'approved', 'denied', 'started',
        'completed', 'failed', 'cancelled', 'outcome_unknown'
    )),
    CHECK (approval IN (
        'not_applicable', 'requested', 'policy', 'yolo', 'embedding',
        'once', 'always', 'denied'
    )),
    CHECK (
        length(arguments_sha256) = 64
        AND arguments_sha256 NOT GLOB '*[^0-9a-f]*'
    ),
    CHECK (
        result_sha256 = '' OR (
            length(result_sha256) = 64
            AND result_sha256 NOT GLOB '*[^0-9a-f]*'
        )
    ),
    CHECK (length(CAST(result_receipt AS BLOB)) <= 16384),
    CHECK (length(CAST(detail AS BLOB)) <= 4096),
    CHECK (length(trim(occurred_at)) > 0),
    CHECK (length(trim(recorded_at)) > 0)
);

-- One immutable row per phase. Exact repeats are handled by the Store API;
-- conflicting repeats fail this constraint.
CREATE UNIQUE INDEX IF NOT EXISTS ux_execution_events_phase
    ON execution_events(execution_id, event_type);

-- An idempotency key identifies one effect lifecycle, not just one tool call.
CREATE UNIQUE INDEX IF NOT EXISTS ux_execution_events_idempotency_phase
    ON execution_events(idempotency_key, event_type);

-- Approval cannot resolve both ways, and an execution has exactly one terminal
-- receipt even if recovery races the original process.
CREATE UNIQUE INDEX IF NOT EXISTS ux_execution_events_approval_decision
    ON execution_events(execution_id)
    WHERE event_type IN ('approved', 'denied');

CREATE UNIQUE INDEX IF NOT EXISTS ux_execution_events_terminal
    ON execution_events(execution_id)
    WHERE event_type IN ('denied', 'completed', 'failed', 'cancelled', 'outcome_unknown');

CREATE UNIQUE INDEX IF NOT EXISTS ux_execution_events_call_request
    ON execution_events(session_id, run_id, canonical_call_id)
    WHERE event_type = 'requested';

CREATE UNIQUE INDEX IF NOT EXISTS ux_execution_events_position_request
    ON execution_events(session_id, run_id, turn_id, iteration, ordinal)
    WHERE event_type = 'requested';

CREATE INDEX IF NOT EXISTS idx_execution_events_session_order
    ON execution_events(workspace_id, session_id, id);

CREATE INDEX IF NOT EXISTS idx_execution_events_execution_order
    ON execution_events(workspace_id, session_id, execution_id, id DESC);

CREATE INDEX IF NOT EXISTS idx_execution_events_run_order
    ON execution_events(workspace_id, session_id, run_id, turn_id, id);

-- A copied session id must never be used to cross workspace scope.
CREATE TRIGGER IF NOT EXISTS execution_events_workspace_guard
BEFORE INSERT ON execution_events
WHEN NOT EXISTS (
    SELECT 1 FROM sessions
    WHERE id = NEW.session_id AND workspace_id = NEW.workspace_id
)
BEGIN
    SELECT RAISE(ABORT, 'execution event workspace does not match session');
END;

CREATE TRIGGER IF NOT EXISTS execution_events_no_update
BEFORE UPDATE ON execution_events
BEGIN
    SELECT RAISE(ABORT, 'execution events are append-only');
END;

-- During ON DELETE CASCADE the parent session has already disappeared, so the
-- predicate preserves explicit session deletion while rejecting direct row
-- deletion.
CREATE TRIGGER IF NOT EXISTS execution_events_no_delete
BEFORE DELETE ON execution_events
WHEN EXISTS (SELECT 1 FROM sessions WHERE id = OLD.session_id)
BEGIN
    SELECT RAISE(ABORT, 'execution events are append-only');
END;
