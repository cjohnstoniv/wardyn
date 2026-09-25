-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- agent_runs.model_provider_id freezes the model provider chooseModelProvider
-- (internal/api/run_model_provider.go, MP-6a #526) resolved this run to at
-- create time -- multi-provider design section 2.4 step 5: "Persist and
-- revalidate". The run row freezes the id alone; the KIND is only recorded on
-- the run.create audit event's model_provider snapshot (#527), because a
-- provider's kind can change later (a kind change mints a fresh UID, #521) and
-- the row would then read a kind the id no longer has.
--
-- Empty for a run under no provider block (today's path, unchanged until #526
-- onward config exists) and for every run created before this column existed.
--
-- NOT NULL DEFAULT '' -- a metadata-only add on PG11+, exactly as
-- 0065_agent_runs_autonomy_level and 0063_agent_runs_status_detail did -- so
-- scanRun keeps scanning into a plain string and every legacy row reads "no
-- provider was chosen" rather than threading a nullable through the agent_runs
-- column lists.
ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS model_provider_id TEXT NOT NULL DEFAULT '';
