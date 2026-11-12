-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- A macOS VM task run's agent output recording, owed. Dispatch sets agent_cast_owed_at when the run's
-- agent output cast opens; a stored cast of the run clears it, and so does marking the run
-- not_recorded runner_declined when no cast could be stored. A terminal run that still owes one is
-- settled by the output sweeper whatever its output row says, so a process that dies mid-save never
-- leaves a run marked recorded with no cast. NULL on every existing row. agent_runs already has its
-- grants.
ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS agent_cast_owed_at TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS agent_runs_agent_cast_owed_idx ON agent_runs (agent_cast_owed_at)
    WHERE agent_cast_owed_at IS NOT NULL;
