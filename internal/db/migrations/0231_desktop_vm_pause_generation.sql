-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0
--
-- The order of a VM run's sign-in pauses (0230): pause_generation counts the run's pauses and the
-- stops wardynd sends again. Each takes the next one, under the run's operation lock, before it asks the
-- runner to stop the agent, and every stop and continue carries it to the guest, which drops a request
-- out of order. So a continue that reaches the guest after a later pause's stop no longer runs the agent while
-- the run page says paused. A resume clears only the mark of the pause and generation it read.
--
-- Existing rows read 0, which is what a run created before this migration has used: its first pause
-- after the upgrade takes 1.
--
-- No table is new, so a split-role install needs no grant change.
--
-- Re-runnable: ADD COLUMN IF NOT EXISTS.

ALTER TABLE desktop_vm_sign_ins ADD COLUMN IF NOT EXISTS pause_generation bigint NOT NULL DEFAULT 0;
