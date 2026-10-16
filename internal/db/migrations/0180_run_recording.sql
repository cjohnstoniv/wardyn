-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Recordings follow the organisation (0.9.3). An organisation and a pool may require every run
-- recorded ('required'); '' and 'preferred' both record a run whenever its organisation and runner
-- can, which is how every organisation and pool behaved before this column. A run says why it has no
-- recording: 'org_off' (the organisation keeps none) or 'runner_declined' (its runner does not record
-- a run on its organisation's word), decided once at dispatch; '' is recorded, or a run that predates it.
ALTER TABLE runner_pool_org_defaults ADD COLUMN IF NOT EXISTS recording TEXT NOT NULL DEFAULT ''
    CHECK (recording IN ('', 'preferred', 'required'));
ALTER TABLE runner_pools ADD COLUMN IF NOT EXISTS recording TEXT NOT NULL DEFAULT ''
    CHECK (recording IN ('', 'preferred', 'required'));
ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS not_recorded TEXT NOT NULL DEFAULT ''
    CHECK (not_recorded IN ('', 'org_off', 'runner_declined'));
