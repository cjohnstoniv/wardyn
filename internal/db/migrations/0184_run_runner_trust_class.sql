-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The trust class a local run's credential plan was admitted at (0.9.3). Create judges the plan at
-- each trust class among the run's eligible runners, highest first, records the first that admits it
-- here and binds the run to a runner of that class; a revive refuses when the bound runner's stored
-- class (runners.trust_class, 0180) is no longer this one. NULL: a remote run, or a local run whose
-- class was never recorded, which a revive refuses. Every runner before 0180 was personal, so a local
-- run already bound to one is recorded personal.
ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS runner_trust_class TEXT
    CHECK (runner_trust_class IS NULL OR runner_trust_class IN ('personal', 'managed_device', 'org_cluster'));
UPDATE agent_runs SET runner_trust_class = 'personal' WHERE runner_id IS NOT NULL AND runner_trust_class IS NULL;
