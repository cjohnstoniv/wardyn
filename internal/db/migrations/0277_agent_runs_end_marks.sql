-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- fenced_at: an ended run whose agent was cut off every network (FenceProxy returned nil). The containment
-- loop never visits it again. revoke_owed_at: a run whose access revoke started and has not yet succeeded;
-- a worker on every replica retries it until it does.
ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS fenced_at timestamptz;
ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS revoke_owed_at timestamptz;
-- The tail owner pages the ended runs that still hold a sandbox, and the containment loop the ones not yet cut
-- off, newest first, every few seconds on every replica; these partial indexes keep those reads on the few such rows.
CREATE INDEX IF NOT EXISTS agent_runs_terminal_sandbox_idx
    ON agent_runs ((COALESCE(ended_at, updated_at)) DESC, id DESC) WHERE sandbox_ref <> '';
CREATE INDEX IF NOT EXISTS agent_runs_unfenced_sandbox_idx
    ON agent_runs ((COALESCE(ended_at, updated_at)) DESC, id DESC) WHERE sandbox_ref <> '' AND fenced_at IS NULL;
CREATE INDEX IF NOT EXISTS agent_runs_revoke_owed_idx ON agent_runs (revoke_owed_at) WHERE revoke_owed_at IS NOT NULL;
-- A copy back given up whose given_up audit row is not yet accepted: the tail owner writes it again,
-- whether or not the run still holds a sandbox.
CREATE INDEX IF NOT EXISTS agent_runs_given_up_unaudited_idx ON agent_runs (id)
    WHERE (local_folder_status->'back'->>'given_up_at') IS NOT NULL
      AND COALESCE((local_folder_status->'back'->>'given_up_audited')::boolean, false) = false;
