-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The governed allowed-models list a run was dispatched under and the profile that set it
-- (api.recordAllowedModels): {"models": [...], "profile": {"id": ..., "name": ...}}. Written at
-- dispatch and never updated when the profile changes; a revive replays it. NULL on a run that no
-- allowed-models limit binds.
ALTER TABLE agent_runs
    ADD COLUMN IF NOT EXISTS allowed_models JSONB;
