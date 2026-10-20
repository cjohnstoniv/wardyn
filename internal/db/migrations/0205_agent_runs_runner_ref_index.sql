-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- store.PG.RunnerRetirementCandidates runs every minute on every replica. It used to start from every
-- runner_output_watermarks row, which a runner run keeps for good, so it joined all runner history to find
-- the few runs that still need retiring. It now starts from the open retirements and from the runner runs
-- still holding a ref; this partial index is how it finds the second set without reading every run.
CREATE INDEX IF NOT EXISTS agent_runs_runner_ref_idx
    ON agent_runs (id) WHERE runner_id IS NOT NULL AND sandbox_ref <> '';
