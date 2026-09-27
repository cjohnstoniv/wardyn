-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- ended_at is the run's end time as the three terminal writers set it (#1197
-- L1a): NULL for a live run. The backfill from updated_at is approximate for
-- historical rows (updated_at has several unrelated writers) but is accepted
-- for history — every writer from this migration forward stamps it exactly at
-- the terminal transition. The partial indexes cover the landing page's
-- end-time ordering and its per-creator variant without indexing the (much
-- larger, and irrelevant to this) NULL/live set.
ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS ended_at TIMESTAMPTZ;

UPDATE agent_runs SET ended_at = updated_at
 WHERE ended_at IS NULL AND state IN ('COMPLETED', 'FAILED', 'KILLED', 'STOPPED', 'ARCHIVED');

CREATE INDEX IF NOT EXISTS agent_runs_ended_at_idx ON agent_runs (ended_at DESC) WHERE ended_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS agent_runs_created_by_ended_at_idx ON agent_runs (created_by, ended_at DESC) WHERE ended_at IS NOT NULL;
