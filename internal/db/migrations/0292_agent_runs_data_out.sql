-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The run's data_out rule (allowed, review or blocked), the one every data_out door reads. Dispatch writes it
-- just before the run.policy.resolve row that records the policy, and a revive whose seal makes the rule
-- stricter raises it before its policy generation is stored. It is never lowered. Audit retention prunes the
-- audit rows and never this column, so a run whose policy rows are pruned keeps its rule. NULL is a run not
-- yet dispatched, which has no sandbox and so no content to keep in: the doors read its recorded policy, and a
-- run with none as allowed. A run from before the column is filled by one of the two UPDATEs below, or stays
-- NULL when neither applies. Nullable with no default, but the inline rule makes Postgres read every existing
-- run once, and the whole file runs in one transaction: the ALTER's exclusive lock on agent_runs is held
-- through that read, the fill's scan of the run.policy.resolve, run.policy.effective and run.policy.generation
-- rows in audit_events, both UPDATEs, and the one look each makes at agent_runs.policy_generations and, for a
-- run past generation 1, at its run.policy.generation rows (the runs it leaves NULL, below).
-- agent_runs already has its grants.
ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS data_out TEXT
    CHECK (data_out IN ('allowed', 'review', 'blocked'));

-- Filled from the policy rows retention has kept: each run gets the strictest rule among its run.policy.resolve
-- row (run.policy.effective in older releases) and its run.policy.generation rows. A resolve row with no rule
-- reads as allowed. A generation row with no rule (a Kubernetes egress edit's, or a revive of a run whose
-- resolve row was gone) says nothing and is skipped, so a run left with only such rows is not filled here. A
-- revive only raises the rule, so the strictest row is at least the rule in force, but only when the row of the
-- run's current policy generation is present: that row is written best-effort, and a run whose row never landed
-- would be filled from older rows, looser than the rule it enforces. A run whose current generation is 2 or
-- later and has no readable row for it (a run.policy.generation row of that number with a policy object) is
-- left NULL here and by the second UPDATE, so the doors answer it as unreadable, as before the column. The
-- current generation is the number of the last policy_generations entry not abandoned, 1 when there is none.
WITH cur AS (SELECT r.id AS run_id,
                    COALESCE((SELECT (e.elem->>'generation')::int
                                FROM jsonb_array_elements(r.policy_generations) WITH ORDINALITY AS e(elem, ord)
                               WHERE COALESCE((e.elem->>'abandoned')::boolean, false) = false
                               ORDER BY e.ord DESC LIMIT 1), 1) AS gen
               FROM agent_runs r),
     unread AS (SELECT c.run_id
                  FROM cur c
                 WHERE c.gen >= 2
                   AND NOT EXISTS (SELECT 1 FROM audit_events g
                                    WHERE g.run_id = c.run_id AND g.action = 'run.policy.generation'
                                      AND CASE WHEN g.action = 'run.policy.generation' THEN (g.data->>'generation')::int END = c.gen
                                      AND jsonb_typeof(g.data->'policy') = 'object'))
UPDATE agent_runs r
   SET data_out = f.rule
  FROM (SELECT a.run_id,
               (ARRAY['allowed', 'review', 'blocked'])[max(array_position(ARRAY['allowed', 'review', 'blocked'],
                   CASE WHEN a.action = 'run.policy.generation' THEN NULLIF(a.data->'policy'->>'data_out', '')
                        ELSE COALESCE(NULLIF(a.data->>'data_out', ''), 'allowed') END))] AS rule
          FROM audit_events a
         WHERE a.run_id IS NOT NULL
           AND a.action IN ('run.policy.resolve', 'run.policy.effective', 'run.policy.generation')
         GROUP BY a.run_id) f
 WHERE r.id = f.run_id AND r.data_out IS NULL AND f.rule IS NOT NULL
   AND r.id NOT IN (SELECT run_id FROM unread);

-- A run still NULL has no policy row with a rule left. One whose policy rows retention dropped, and which was
-- dispatched, may hold content whose rule is unknown, so it is set to blocked rather than read as allowed.
--   Dropped: retention removes audit rows only as whole partitions, oldest first by recorded_at, and records
--   each drop in audit_chain_anchors (kind 'drop', recorded_hi = the partition's largest recorded_at). A
--   resolve row is written after the run is created, so a run whose resolve row was dropped was created before
--   the newest dropped recorded_hi. created_at is the server's clock and recorded_at the database's, so the
--   test allows a day for skew between them. With no drop recorded nothing was ever removed: a run left with
--   no rule never wrote one, and stays NULL.
--   Dispatched: dispatch writes the resolve row after it claims the run out of PENDING (state STARTING) and
--   before the sandbox exists (sandbox_ref, set once it does). A run still QUEUED, PENDING or STARTING with no
--   sandbox has not reached that row: it stays NULL, and its dispatch writes its rule (a previous release's,
--   the resolve row). An ended run's sandbox_ref is cleared, so one that failed or was killed before that row
--   is set to blocked too when it is that old; it holds no content to lose.
--   Unread: a run whose current generation's row is missing (the first UPDATE's) is not set to blocked
--   either: blocked would stop its sandbox at the run's end and lose its computer folder's changes, while
--   NULL makes every door answer it unreadable and keeps the sandbox.
WITH cur AS (SELECT r.id AS run_id,
                    COALESCE((SELECT (e.elem->>'generation')::int
                                FROM jsonb_array_elements(r.policy_generations) WITH ORDINALITY AS e(elem, ord)
                               WHERE COALESCE((e.elem->>'abandoned')::boolean, false) = false
                               ORDER BY e.ord DESC LIMIT 1), 1) AS gen
               FROM agent_runs r),
     unread AS (SELECT c.run_id
                  FROM cur c
                 WHERE c.gen >= 2
                   AND NOT EXISTS (SELECT 1 FROM audit_events g
                                    WHERE g.run_id = c.run_id AND g.action = 'run.policy.generation'
                                      AND CASE WHEN g.action = 'run.policy.generation' THEN (g.data->>'generation')::int END = c.gen
                                      AND jsonb_typeof(g.data->'policy') = 'object'))
UPDATE agent_runs
   SET data_out = 'blocked'
 WHERE data_out IS NULL
   AND id NOT IN (SELECT run_id FROM unread)
   AND NOT (state IN ('QUEUED', 'PENDING', 'STARTING') AND sandbox_ref = '')
   AND created_at < (SELECT max(recorded_hi) FROM audit_chain_anchors WHERE kind = 'drop') + interval '1 day';
