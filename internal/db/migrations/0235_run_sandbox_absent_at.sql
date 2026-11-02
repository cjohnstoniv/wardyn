-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- When a runner's STATE inventory first reported a RUNNING run's sandbox absent with no exit code,
-- since the last report that said otherwise. A later fresh session's report that it is still absent,
-- at least store.RunnerSandboxGoneAfter after this time, ends the run lost (gone); any other report
-- of the run clears it. NULL for every existing row. agent_runs exists already (0001), so no grant
-- changes; lost_reason keeps no CHECK (0073).
ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS sandbox_absent_at timestamptz;
