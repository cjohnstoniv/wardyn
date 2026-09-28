-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The run's resolved ephemeral disk cap (long-holds design rev 4, RL-13),
-- captured the same way image is: applyEphemeralDisk (runs_dispatch_ceiling.go)
-- decides the effective disk_mib AFTER the row is inserted (it needs the
-- site config and the ceiling, both dispatch-time inputs), so this is a scoped
-- UPDATE (SetRunDiskMiB), not a CreateRun column value. The run page's disk-used
-- reading uses it as the 80%-warning denominator, never re-derived later: a
-- site-config or profile change after dispatch must not move a live run's cap
-- out from under it, the same rule ends_at and run_limits already follow.
--
-- 0 = no cap resolved (every legacy row, and any run whose disk stayed
-- unbounded) — read as "no cap", not as a zero-byte quota.
ALTER TABLE agent_runs
    ADD COLUMN IF NOT EXISTS disk_mib INTEGER NOT NULL DEFAULT 0;
