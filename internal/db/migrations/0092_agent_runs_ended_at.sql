-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- ended_at is the run's end time as the three terminal writers set it (#1197
-- L1a): NULL for a live run. The backfill from updated_at is approximate for
-- historical rows (updated_at has several unrelated writers) but is accepted
-- for history — every writer from this migration forward stamps it exactly at
-- the terminal transition.
--
-- No index on the plain column: the landing page's ordering and windowing
-- read `CASE WHEN lost_reason='ended' THEN lost_at ELSE ended_at END`, not
-- ended_at alone, so a plain (or partial) btree on ended_at cannot serve
-- either read — confirmed by EXPLAIN ANALYZE against a 200k-row seed. Add an
-- expression index on that CASE if a real deployment's EXPLAIN ever asks for
-- one; today's owner-scoped scan already rides agent_runs_created_by_idx.
ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS ended_at TIMESTAMPTZ;

UPDATE agent_runs SET ended_at = updated_at
 WHERE ended_at IS NULL AND state IN ('COMPLETED', 'FAILED', 'KILLED', 'STOPPED', 'ARCHIVED');
