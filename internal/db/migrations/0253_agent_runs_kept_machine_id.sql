-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The kept machine a macOS VM run kept or resumed, and how it got it (new, resume or fresh). Dispatch sets
-- both when it inserts or claims the machine. No foreign key: the id stays after the machine's row is
-- deleted, so the run's own page can say the machine was deleted. NULL on every existing row. agent_runs
-- already has its grants.
ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS kept_machine_id UUID;
ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS kept_machine_mode TEXT;
