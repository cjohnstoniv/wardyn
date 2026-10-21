-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The model allowlist each local model provider handed the run's proxy at its latest dispatch
-- (api.recordModelAllowlists): {provider id: [model, ...]}. The proxy holds a run to that list
-- while the provider's own list may change later, so the run page reads this record. NULL on a
-- run that has not dispatched on a local model provider.
ALTER TABLE agent_runs
    ADD COLUMN IF NOT EXISTS model_allowlists JSONB;
