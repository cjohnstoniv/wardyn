-- Extend the audit_events append-only guarantee to cover TRUNCATE.
--
-- The 0001 trigger fires BEFORE UPDATE OR DELETE ... FOR EACH ROW, which blocks
-- row mutation/deletion but NOT `TRUNCATE audit_events` (TRUNCATE is a
-- statement-level DDL that bypasses row triggers). The docs state the audit log
-- is append-only / "UPDATE and DELETE are blocked", so a silent TRUNCATE wipe is
-- an integrity gap. Add a statement-level BEFORE TRUNCATE trigger that raises,
-- reusing the same exception function.

DROP TRIGGER IF EXISTS audit_events_no_truncate ON audit_events;
CREATE TRIGGER audit_events_no_truncate
    BEFORE TRUNCATE ON audit_events
    FOR EACH STATEMENT EXECUTE FUNCTION audit_events_append_only();
