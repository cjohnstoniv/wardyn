-- Add the terminal state CANCELLED to the approvals.state CHECK.
--
-- A run's terminal transition (kill, completion, failure, stop) now cancels the
-- run's still-PENDING approvals: the question they asked cannot be answered any
-- more, and the console used to render live Approve/Deny buttons on a run the
-- same screen labelled Killed. CANCELLED is its own value rather than DENIED
-- (nobody refused it) or EXPIRED (no sweeper aged it out) -- see
-- internal/types/types.go ApprovalCancelled and approval.CancelForRun.
--
-- The 0001 CHECK is an inline column CHECK, so Postgres named it
-- "approvals_state_check"; this REPLACES it rather than adding a second one.
-- Two CHECKs on one column are ANDed, which would leave the admitted set as
-- their intersection -- 'CANCELLED' rejected by the first constraint, with
-- every gate green (the enum-parity guard reads the LAST definition, and
-- internal/db/migrations_check_pg_test.go's liveCheckDef refuses two CHECKs on
-- one column outright). 0003_run_state_completed.sql is the precedent for this
-- drop-then-add shape; drop-if-exists keeps it idempotent and tolerant of an
-- environment where the constraint name differs.

ALTER TABLE approvals DROP CONSTRAINT IF EXISTS approvals_state_check;

ALTER TABLE approvals ADD CONSTRAINT approvals_state_check CHECK (state IN (
    'PENDING',
    'APPROVED',
    'DENIED',
    'EXPIRED',
    'CANCELLED'
));
