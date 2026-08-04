-- Reconciliation is an overlay on the immutable execution ledger. One
-- execution lifecycle can acquire at most one authority item, regardless of
-- which goal or turn-level recovery path observed it.
CREATE UNIQUE INDEX IF NOT EXISTS ux_control_items_execution_reconciliation_target
    ON control_items(session_id, workspace_id, execution_id)
    WHERE kind = 'execution_reconciliation';

-- Direct SQL and Store callers must bind an execution-reconciliation item to
-- the exact immutable turn and current hazardous state. A later completed
-- receipt can never acquire (or be hidden by) a reconciliation item.
CREATE TRIGGER IF NOT EXISTS control_items_execution_reconciliation_target_guard
BEFORE INSERT ON control_items
WHEN NEW.kind = 'execution_reconciliation'
 AND NOT EXISTS (
    SELECT 1
      FROM execution_events e
     WHERE e.session_id = NEW.session_id
       AND e.workspace_id = NEW.workspace_id
       AND e.execution_id = NEW.execution_id
       AND e.turn_id = NEW.turn_id
       AND e.id = (
           SELECT MAX(latest.id)
             FROM execution_events latest
            WHERE latest.session_id = NEW.session_id
              AND latest.workspace_id = NEW.workspace_id
              AND latest.execution_id = NEW.execution_id
       )
       AND (
           e.event_type = 'outcome_unknown'
           OR (e.event_type = 'started' AND e.effect_class != 'read_only')
       )
 )
BEGIN
    SELECT RAISE(ABORT, 'execution reconciliation target is not the exact latest hazardous turn');
END;
