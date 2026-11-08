-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The runs frozen to a governance unit, for a unit admin's audit feed (the rows of the runs in their
-- subtrees). A run with no unit never matches, so it stays out of the index.
CREATE INDEX IF NOT EXISTS agent_runs_unit_idx ON agent_runs (unit_id) WHERE unit_id IS NOT NULL;
