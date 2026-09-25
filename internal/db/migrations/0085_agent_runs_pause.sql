-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Pause and resume (long-holds design rev 4, section 3, RL-7). A run nobody is
-- using has its agent container frozen in place; it keeps its RunState
-- (RUNNING), its memory, its files and its proxy, and carries the substate
-- here instead, for the same reason migration 0073 gives for lost_at.
--
-- paused_at / paused_reason: when and why the agent was frozen. NULL / '' is
-- a run that is not paused, which is every legacy row. The reason is a closed
-- Go vocabulary (waiting, idle) validated at the write boundary, so no CHECK
-- backs it (the 0042 doctrine).
--
-- active_at: the presence clock. It moves on input a person types into the
-- run, on the agent's egress decisions and on bytes the proxy moves for it;
-- keepalives move only updated_at. NULL (every legacy row) reads as the run's
-- created_at.
--
-- All three adds are metadata-only on PG11+.
ALTER TABLE agent_runs
    ADD COLUMN IF NOT EXISTS paused_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS paused_reason TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS active_at TIMESTAMPTZ;
