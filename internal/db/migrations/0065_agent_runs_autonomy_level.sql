-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- agent_runs.autonomy_level freezes the AutonomyLevel (L0-L3) a run was
-- resolved to at create time -- internal/types/governance.go's AutonomyLevel
-- and AutonomyRubric (0.8, #77/#99). Nothing writes it yet: resolving and
-- enforcing the level is #97, landing separately so that gate has one shape
-- to work with. This migration exists so the column, the insert list and
-- scanRun are settled before the resolution logic depends on them.
--
-- NOT NULL DEFAULT '' -- a metadata-only add on PG11+, exactly as
-- 0044_run_failure_hint and 0063_agent_runs_status_detail did -- so scanRun
-- keeps scanning into a plain string and every legacy row reads "no level
-- was resolved" rather than threading a nullable through the agent_runs
-- column lists.
ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS autonomy_level TEXT NOT NULL DEFAULT '';
