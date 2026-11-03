-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The kept machine a macOS VM run kept or resumed, and how it got it (new, resume or fresh). Dispatch sets
-- both when it inserts or claims the machine. No foreign key: the id stays after the machine's row is
-- deleted, so the run's own page can say the machine was deleted. NULL on every existing row.
ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS kept_machine_id UUID;
ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS kept_machine_mode TEXT;

-- The run whose fresh start of the machine was asked of its Mac and not yet confirmed. Set just before the
-- runner is asked to reset the machine; cleared with the new base once the sandbox is up. While set, the
-- machine's disk may or may not have been reset, so only another fresh start may use it. Both tables
-- already have their grants.
ALTER TABLE kept_machines ADD COLUMN IF NOT EXISTS reset_run_id UUID;

-- An earlier fresh start that was asked of the Mac and never confirmed, kept when a later attempt is
-- marked and then taken back because its Mac was never asked. Cleared only by a confirmed fresh start.
ALTER TABLE kept_machines ADD COLUMN IF NOT EXISTS reset_unconfirmed BOOLEAN NOT NULL DEFAULT false;
