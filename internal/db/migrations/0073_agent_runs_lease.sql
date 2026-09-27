-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The lease (long-holds design rev 4, RL-3). At ends_at a run stops and is
-- KEPT: its agent container is stopped, its proxy removed and its broker
-- credentials revoked, but its files stay for WARDYN_ENDED_RUN_GRACE. The run
-- keeps its RunState (RUNNING) and carries the substate here instead, because
-- a new RunState would widen the attach gate, the reaper scan and migration
-- 0027's literal list.
--
-- lost_at / lost_reason: when and why the run lost its sandbox. 'ended' (the
-- lease ran out) is the only reason written today; the design shares the pair
-- with the reboot/outage/node reasons that come later. NULL / '' = a live run,
-- which is every legacy row. The reason is a closed Go vocabulary validated at
-- the write boundary, so no CHECK backs it (the 0042 doctrine).
--
-- ending_soon_for / ending_soon_sec: the run.ending_soon warnings already sent,
-- as the end they were counted against and the smallest threshold sent. Keyed
-- on the end so that any change to ends_at re-arms the warnings with no second
-- write to remember.
--
-- All four adds are metadata-only on PG11+.
ALTER TABLE agent_runs
    ADD COLUMN IF NOT EXISTS lost_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS lost_reason TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS ending_soon_for TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS ending_soon_sec INTEGER NOT NULL DEFAULT 0;
