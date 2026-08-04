-- Durable exception/control-plane items are independent of execution_events.
-- They record questions that need authority, then append one evidence-backed
-- resolution without rewriting either the question or the execution ledger.
CREATE TABLE IF NOT EXISTS control_items (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    item_id           TEXT    NOT NULL UNIQUE,
    idempotency_key   TEXT    NOT NULL UNIQUE,
    session_id        INTEGER NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    workspace_id      TEXT    NOT NULL,
    kind              TEXT    NOT NULL,
    goal_id           TEXT    NOT NULL DEFAULT '',
    execution_id      TEXT    NOT NULL DEFAULT '',
    turn_id           TEXT    NOT NULL DEFAULT '',
    external_id       TEXT    NOT NULL DEFAULT '',
    summary           TEXT    NOT NULL,
    payload_json      TEXT    NOT NULL,
    payload_sha256    TEXT    NOT NULL,
    created_at        TEXT    NOT NULL,
    recorded_at       TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),

    CHECK (session_id > 0),
    CHECK (length(trim(item_id)) > 0 AND length(CAST(item_id AS BLOB)) <= 128),
    CHECK (length(trim(idempotency_key)) > 0 AND length(CAST(idempotency_key AS BLOB)) <= 128),
    CHECK (length(trim(workspace_id)) > 0 AND length(CAST(workspace_id AS BLOB)) <= 4096),
    CHECK (kind IN ('cortex_decision', 'deferred_approval', 'execution_reconciliation')),
    CHECK (length(CAST(goal_id AS BLOB)) <= 128),
    CHECK (length(CAST(execution_id AS BLOB)) <= 128),
    CHECK (length(CAST(turn_id AS BLOB)) <= 128),
    CHECK (kind != 'execution_reconciliation' OR length(trim(execution_id)) > 0),
    CHECK (length(CAST(external_id AS BLOB)) <= 1024),
    CHECK (length(trim(summary)) > 0 AND length(CAST(summary AS BLOB)) <= 4096),
    CHECK (json_valid(payload_json) AND json_type(payload_json) = 'object'),
    CHECK (length(CAST(payload_json AS BLOB)) <= 16384),
    CHECK (
        length(payload_sha256) = 64
        AND payload_sha256 NOT GLOB '*[^0-9a-f]*'
    ),
    CHECK (length(trim(created_at)) > 0),
    CHECK (length(trim(recorded_at)) > 0)
);

CREATE INDEX IF NOT EXISTS idx_control_items_session_order
    ON control_items(workspace_id, session_id, id);

CREATE INDEX IF NOT EXISTS idx_control_items_goal_order
    ON control_items(workspace_id, session_id, goal_id, id)
    WHERE goal_id != '';

CREATE INDEX IF NOT EXISTS idx_control_items_execution_order
    ON control_items(workspace_id, session_id, execution_id, id)
    WHERE execution_id != '';

CREATE INDEX IF NOT EXISTS idx_control_items_turn_order
    ON control_items(workspace_id, session_id, turn_id, id)
    WHERE turn_id != '';

CREATE TABLE IF NOT EXISTS control_resolutions (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    resolution_id     TEXT    NOT NULL UNIQUE,
    idempotency_key   TEXT    NOT NULL UNIQUE,
    item_id           TEXT    NOT NULL UNIQUE REFERENCES control_items(item_id) ON DELETE CASCADE,
    session_id        INTEGER NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    workspace_id      TEXT    NOT NULL,
    outcome           TEXT    NOT NULL,
    evidence_json     TEXT    NOT NULL,
    evidence_sha256   TEXT    NOT NULL,
    resolved_by       TEXT    NOT NULL,
    detail            TEXT    NOT NULL DEFAULT '',
    resolved_at       TEXT    NOT NULL,
    recorded_at       TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),

    CHECK (session_id > 0),
    CHECK (length(trim(resolution_id)) > 0 AND length(CAST(resolution_id AS BLOB)) <= 128),
    CHECK (length(trim(idempotency_key)) > 0 AND length(CAST(idempotency_key AS BLOB)) <= 128),
    CHECK (length(trim(item_id)) > 0 AND length(CAST(item_id AS BLOB)) <= 128),
    CHECK (length(trim(workspace_id)) > 0 AND length(CAST(workspace_id AS BLOB)) <= 4096),
    CHECK (outcome IN ('answered', 'approved', 'denied', 'reconciled', 'dismissed')),
    CHECK (json_valid(evidence_json) AND json_type(evidence_json) = 'object'),
    CHECK (length(CAST(evidence_json AS BLOB)) <= 16384),
    CHECK (
        length(evidence_sha256) = 64
        AND evidence_sha256 NOT GLOB '*[^0-9a-f]*'
    ),
    CHECK (length(trim(resolved_by)) > 0 AND length(CAST(resolved_by AS BLOB)) <= 512),
    CHECK (length(CAST(detail AS BLOB)) <= 4096),
    CHECK (length(trim(resolved_at)) > 0),
    CHECK (length(trim(recorded_at)) > 0)
);

CREATE INDEX IF NOT EXISTS idx_control_resolutions_session_order
    ON control_resolutions(workspace_id, session_id, id);

-- A copied session ID may not cross workspace scope.
CREATE TRIGGER IF NOT EXISTS control_items_workspace_guard
BEFORE INSERT ON control_items
WHEN NOT EXISTS (
    SELECT 1 FROM sessions
    WHERE id = NEW.session_id AND workspace_id = NEW.workspace_id
)
BEGIN
    SELECT RAISE(ABORT, 'control item workspace does not match session');
END;

-- The copied resolution scope must match both its session and its immutable
-- parent item. This keeps direct SQL from bypassing Store scope validation.
CREATE TRIGGER IF NOT EXISTS control_resolutions_scope_guard
BEFORE INSERT ON control_resolutions
WHEN NOT EXISTS (
    SELECT 1
      FROM control_items i
      JOIN sessions s ON s.id = i.session_id AND s.workspace_id = i.workspace_id
     WHERE i.item_id = NEW.item_id
       AND i.session_id = NEW.session_id
       AND i.workspace_id = NEW.workspace_id
)
BEGIN
    SELECT RAISE(ABORT, 'control resolution scope does not match item');
END;

-- Outcome compatibility is also enforced in SQL so a raw writer cannot mark
-- an unknown external execution as dismissed or approved.
CREATE TRIGGER IF NOT EXISTS control_resolutions_outcome_guard
BEFORE INSERT ON control_resolutions
WHEN NOT EXISTS (
    SELECT 1
      FROM control_items i
     WHERE i.item_id = NEW.item_id
       AND (
           (i.kind = 'cortex_decision' AND NEW.outcome IN ('answered', 'dismissed'))
           OR (i.kind = 'deferred_approval' AND NEW.outcome IN ('approved', 'denied', 'dismissed'))
           OR (i.kind = 'execution_reconciliation' AND NEW.outcome = 'reconciled')
       )
)
BEGIN
    SELECT RAISE(ABORT, 'control resolution outcome does not match item kind');
END;

CREATE TRIGGER IF NOT EXISTS control_items_no_update
BEFORE UPDATE ON control_items
BEGIN
    SELECT RAISE(ABORT, 'control items are append-only');
END;

CREATE TRIGGER IF NOT EXISTS control_items_no_delete
BEFORE DELETE ON control_items
WHEN EXISTS (SELECT 1 FROM sessions WHERE id = OLD.session_id)
BEGIN
    SELECT RAISE(ABORT, 'control items are append-only');
END;

CREATE TRIGGER IF NOT EXISTS control_resolutions_no_update
BEFORE UPDATE ON control_resolutions
BEGIN
    SELECT RAISE(ABORT, 'control resolutions are append-only');
END;

CREATE TRIGGER IF NOT EXISTS control_resolutions_no_delete
BEFORE DELETE ON control_resolutions
WHEN EXISTS (SELECT 1 FROM sessions WHERE id = OLD.session_id)
BEGIN
    SELECT RAISE(ABORT, 'control resolutions are append-only');
END;
