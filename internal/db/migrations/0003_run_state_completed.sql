-- Add the terminal-success state COMPLETED to the agent_runs.state CHECK.
--
-- The 0001 CHECK omitted 'COMPLETED', but the control plane's completion
-- watcher transitions a healthy run RUNNING -> COMPLETED on a clean (exit 0)
-- agent exit (see internal/types/types.go RunCompleted and the dispatch
-- watcher in internal/api/runs.go). Without this state in the constraint, the
-- UPDATE was rejected by Postgres, the run never reached a terminal state, and
-- the kill/revoke cascade (revokeRunCascade) never fired -- so credentials
-- minted for a finished run were never revoked. This migration closes that gap.
--
-- The constraint name is the Postgres default for an inline column CHECK on
-- agent_runs.state ("agent_runs_state_check"); drop-if-exists keeps this
-- idempotent and tolerant of environments where the name differs.

ALTER TABLE agent_runs DROP CONSTRAINT IF EXISTS agent_runs_state_check;

ALTER TABLE agent_runs ADD CONSTRAINT agent_runs_state_check CHECK (state IN (
    'PENDING',
    'STARTING',
    'RUNNING',
    'WAITING_FOR_CONFIRMATION',
    'COMPLETED',
    'STOPPED',
    'ARCHIVED',
    'FAILED',
    'KILLED'
));
