-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- A run whose agent container may not match its pause mark (0097). A pause
-- whose mark loses its compare thaws its own freeze, and under HA it can lose
-- its run lock while it does: a newer pause may then be thawed, or the thaw
-- cannot be made at all. The compensation writes this row before it acts and
-- deletes it only once it has read the run and moved the container to its mark
-- with the lock held throughout. Whatever it leaves, the pause sweep settles
-- from due_at on: it reads the run under its lock and freezes a paused run,
-- thaws one that is not, and deletes the row. due_at is past the compensation's
-- own deadline, so the sweep never acts beside the compensation that wrote it.
-- A compensation that writes again moves due_at forward, and a delete names the
-- due_at it read, so it never drops a later one.
--
-- Split-role installs grant the app role SELECT, INSERT, UPDATE, DELETE on
-- run_pause_settles.
CREATE TABLE IF NOT EXISTS run_pause_settles (
    run_id UUID        PRIMARY KEY REFERENCES agent_runs(id) ON DELETE CASCADE,
    due_at TIMESTAMPTZ NOT NULL
);
