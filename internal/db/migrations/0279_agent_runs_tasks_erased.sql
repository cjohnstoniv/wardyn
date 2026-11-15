-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- tasks_erased_at: set by a person's run_tasks erasure on each of their runs still waiting in a queue or
-- promoted from one. A promoted start holds the run's queue row in memory, task text included, and puts it
-- back when its start has to wait again; a run with this set never goes back, so that text is not restored.
ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS tasks_erased_at timestamptz;
