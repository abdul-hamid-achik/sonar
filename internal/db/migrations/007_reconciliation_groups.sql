-- A reconciliation group is the turn-level authority for one blocked goal
-- turn. It is deliberately separate from control_items so adding the parent
-- kind does not rebuild or weaken the existing append-only CHECK table.
CREATE TABLE IF NOT EXISTS reconciliation_groups (
    id                     INTEGER PRIMARY KEY AUTOINCREMENT,
    group_item_id          TEXT    NOT NULL UNIQUE,
    idempotency_key        TEXT    NOT NULL UNIQUE,
    session_id             INTEGER NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    workspace_id           TEXT    NOT NULL,
    goal_id                TEXT    NOT NULL,
    turn_id                TEXT    NOT NULL,
    blocker_reference      TEXT    NOT NULL,
    snapshot_cursor        INTEGER NOT NULL,
    goal_snapshot_sha256   TEXT    NOT NULL,
    member_set_sha256      TEXT    NOT NULL,
    execution_member_count INTEGER NOT NULL,
    payload_json           TEXT    NOT NULL,
    payload_sha256         TEXT    NOT NULL,
    created_at             TEXT    NOT NULL,
    recorded_at            TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),

    CHECK (session_id > 0),
    CHECK (length(trim(group_item_id)) > 0 AND length(CAST(group_item_id AS BLOB)) <= 128),
    CHECK (length(trim(idempotency_key)) > 0 AND length(CAST(idempotency_key AS BLOB)) <= 128),
    CHECK (length(trim(workspace_id)) > 0 AND length(CAST(workspace_id AS BLOB)) <= 4096),
    CHECK (length(trim(goal_id)) > 0 AND length(CAST(goal_id AS BLOB)) <= 128),
    CHECK (length(trim(turn_id)) > 0 AND length(CAST(turn_id AS BLOB)) <= 128),
    CHECK (length(trim(blocker_reference)) > 0 AND length(CAST(blocker_reference AS BLOB)) <= 256),
    CHECK (snapshot_cursor >= 0),
    -- The Goal receipt's 10,000-target ceiling also includes the required
    -- turn-level parent, leaving at most 9,999 execution members.
    CHECK (execution_member_count >= 0 AND execution_member_count <= 9999),
    CHECK (length(goal_snapshot_sha256) = 64 AND goal_snapshot_sha256 NOT GLOB '*[^0-9a-f]*'),
    CHECK (length(member_set_sha256) = 64 AND member_set_sha256 NOT GLOB '*[^0-9a-f]*'),
    CHECK (json_valid(payload_json) AND json_type(payload_json) = 'object'),
    CHECK (length(CAST(payload_json AS BLOB)) <= 16384),
    CHECK (length(payload_sha256) = 64 AND payload_sha256 NOT GLOB '*[^0-9a-f]*'),
    CHECK (length(trim(created_at)) > 0),
    CHECK (length(trim(recorded_at)) > 0),
    UNIQUE (session_id, workspace_id, goal_id, turn_id)
);

CREATE INDEX IF NOT EXISTS idx_reconciliation_groups_session
    ON reconciliation_groups(workspace_id, session_id, id DESC);

CREATE TABLE IF NOT EXISTS reconciliation_group_members (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    group_item_id       TEXT    NOT NULL REFERENCES reconciliation_groups(group_item_id) ON DELETE CASCADE,
    control_item_id     TEXT    NOT NULL UNIQUE REFERENCES control_items(item_id) ON DELETE CASCADE,
    execution_id        TEXT    NOT NULL UNIQUE,
    turn_id             TEXT    NOT NULL,
    event_id            INTEGER NOT NULL UNIQUE REFERENCES execution_events(id) ON DELETE CASCADE,
    event_type          TEXT    NOT NULL,
    effect_class        TEXT    NOT NULL,
    event_sha256        TEXT    NOT NULL,
    item_payload_sha256 TEXT    NOT NULL,
    recorded_at         TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),

    CHECK (length(trim(group_item_id)) > 0 AND length(CAST(group_item_id AS BLOB)) <= 128),
    CHECK (length(trim(control_item_id)) > 0 AND length(CAST(control_item_id AS BLOB)) <= 128),
    CHECK (length(trim(execution_id)) > 0 AND length(CAST(execution_id AS BLOB)) <= 128),
    CHECK (length(trim(turn_id)) > 0 AND length(CAST(turn_id AS BLOB)) <= 128),
    CHECK (event_id > 0),
    CHECK (event_type IN ('started', 'outcome_unknown')),
    CHECK (effect_class IN ('read_only', 'effectful', 'unknown')),
    CHECK (event_type != 'started' OR effect_class != 'read_only'),
    CHECK (length(event_sha256) = 64 AND event_sha256 NOT GLOB '*[^0-9a-f]*'),
    CHECK (length(item_payload_sha256) = 64 AND item_payload_sha256 NOT GLOB '*[^0-9a-f]*'),
    UNIQUE (group_item_id, execution_id)
);

CREATE INDEX IF NOT EXISTS idx_reconciliation_group_members_group
    ON reconciliation_group_members(group_item_id, id);

CREATE TABLE IF NOT EXISTS reconciliation_group_resolutions (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    resolution_id   TEXT    NOT NULL UNIQUE,
    idempotency_key TEXT    NOT NULL UNIQUE,
    group_item_id   TEXT    NOT NULL UNIQUE REFERENCES reconciliation_groups(group_item_id) ON DELETE CASCADE,
    session_id      INTEGER NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    workspace_id    TEXT    NOT NULL,
    evidence_json   TEXT    NOT NULL,
    evidence_sha256 TEXT    NOT NULL,
    resolved_by     TEXT    NOT NULL,
    resolved_at     TEXT    NOT NULL,
    recorded_at     TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),

    CHECK (session_id > 0),
    CHECK (length(trim(resolution_id)) > 0 AND length(CAST(resolution_id AS BLOB)) <= 128),
    CHECK (length(trim(idempotency_key)) > 0 AND length(CAST(idempotency_key AS BLOB)) <= 128),
    CHECK (length(trim(group_item_id)) > 0 AND length(CAST(group_item_id AS BLOB)) <= 128),
    CHECK (length(trim(workspace_id)) > 0 AND length(CAST(workspace_id AS BLOB)) <= 4096),
    CHECK (json_valid(evidence_json) AND json_type(evidence_json) = 'object'),
    CHECK (length(CAST(evidence_json AS BLOB)) <= 16384),
    CHECK (length(evidence_sha256) = 64 AND evidence_sha256 NOT GLOB '*[^0-9a-f]*'),
    CHECK (length(trim(resolved_by)) > 0 AND length(CAST(resolved_by AS BLOB)) <= 512),
    CHECK (length(trim(resolved_at)) > 0),
    CHECK (length(trim(recorded_at)) > 0)
);

CREATE TRIGGER IF NOT EXISTS reconciliation_groups_scope_guard
BEFORE INSERT ON reconciliation_groups
WHEN NOT EXISTS (
    SELECT 1 FROM sessions
     WHERE id = NEW.session_id AND workspace_id = NEW.workspace_id
)
BEGIN
    SELECT RAISE(ABORT, 'reconciliation group workspace does not match session');
END;

CREATE TRIGGER IF NOT EXISTS reconciliation_group_members_target_guard
BEFORE INSERT ON reconciliation_group_members
WHEN NOT EXISTS (
    SELECT 1
      FROM reconciliation_groups g
      JOIN control_items i
        ON i.item_id = NEW.control_item_id
       AND i.kind = 'execution_reconciliation'
       AND i.session_id = g.session_id
       AND i.workspace_id = g.workspace_id
       AND i.goal_id = g.goal_id
       AND i.turn_id = g.turn_id
       AND i.execution_id = NEW.execution_id
       AND i.payload_sha256 = NEW.item_payload_sha256
      JOIN execution_events e
        ON e.id = NEW.event_id
       AND e.session_id = g.session_id
       AND e.workspace_id = g.workspace_id
       AND e.turn_id = g.turn_id
       AND e.execution_id = NEW.execution_id
       AND e.event_type = NEW.event_type
       AND e.effect_class = NEW.effect_class
     WHERE g.group_item_id = NEW.group_item_id
       AND NEW.turn_id = g.turn_id
       AND e.id = (
           SELECT MAX(latest.id)
             FROM execution_events latest
            WHERE latest.session_id = g.session_id
              AND latest.workspace_id = g.workspace_id
              AND latest.execution_id = NEW.execution_id
       )
)
BEGIN
    SELECT RAISE(ABORT, 'reconciliation group member target is not exact');
END;

CREATE TRIGGER IF NOT EXISTS reconciliation_group_resolutions_scope_guard
BEFORE INSERT ON reconciliation_group_resolutions
WHEN NOT EXISTS (
    SELECT 1
      FROM reconciliation_groups g
     WHERE g.group_item_id = NEW.group_item_id
       AND g.session_id = NEW.session_id
       AND g.workspace_id = NEW.workspace_id
)
BEGIN
    SELECT RAISE(ABORT, 'reconciliation group resolution scope does not match group');
END;

CREATE TRIGGER IF NOT EXISTS reconciliation_groups_no_update
BEFORE UPDATE ON reconciliation_groups BEGIN
    SELECT RAISE(ABORT, 'reconciliation groups are append-only');
END;

CREATE TRIGGER IF NOT EXISTS reconciliation_groups_no_delete
BEFORE DELETE ON reconciliation_groups
WHEN EXISTS (SELECT 1 FROM sessions WHERE id = OLD.session_id) BEGIN
    SELECT RAISE(ABORT, 'reconciliation groups are append-only');
END;

CREATE TRIGGER IF NOT EXISTS reconciliation_group_members_no_update
BEFORE UPDATE ON reconciliation_group_members BEGIN
    SELECT RAISE(ABORT, 'reconciliation group members are append-only');
END;

CREATE TRIGGER IF NOT EXISTS reconciliation_group_members_no_delete
BEFORE DELETE ON reconciliation_group_members
WHEN EXISTS (
    SELECT 1 FROM reconciliation_groups WHERE group_item_id = OLD.group_item_id
) BEGIN
    SELECT RAISE(ABORT, 'reconciliation group members are append-only');
END;

CREATE TRIGGER IF NOT EXISTS reconciliation_group_resolutions_no_update
BEFORE UPDATE ON reconciliation_group_resolutions BEGIN
    SELECT RAISE(ABORT, 'reconciliation group resolutions are append-only');
END;

CREATE TRIGGER IF NOT EXISTS reconciliation_group_resolutions_no_delete
BEFORE DELETE ON reconciliation_group_resolutions
WHEN EXISTS (SELECT 1 FROM sessions WHERE id = OLD.session_id) BEGIN
    SELECT RAISE(ABORT, 'reconciliation group resolutions are append-only');
END;
