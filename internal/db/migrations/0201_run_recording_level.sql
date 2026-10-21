-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Recording is a level (0.10): 'off', 'terminal' or 'screen_actions', resolved once at dispatch from
-- the run's policy and the organisation's level. '' is a run dispatched before the level existed, or
-- one resolved to terminal, and reads as terminal. A run resolved to off says so: not_recorded
-- 'policy_off'.
--
-- Split-role installs grant the app role no extra privilege here: this is a column on a table it
-- already writes.
ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS recording_level TEXT NOT NULL DEFAULT ''
    CHECK (recording_level IN ('', 'off', 'terminal', 'screen_actions'));
ALTER TABLE agent_runs DROP CONSTRAINT IF EXISTS agent_runs_not_recorded_check;
ALTER TABLE agent_runs ADD CONSTRAINT agent_runs_not_recorded_check
    CHECK (not_recorded IN ('', 'org_off', 'runner_declined', 'policy_off'));
