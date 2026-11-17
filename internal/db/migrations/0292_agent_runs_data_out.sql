-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The run's data_out rule (allowed, review or blocked), the one every data_out door reads. Dispatch writes it
-- just before the run.policy.resolve row that records the policy, and a revive whose seal makes the rule
-- stricter raises it before its policy generation is stored. It is never lowered. Audit retention prunes the
-- audit rows and never this column, so a run whose policy rows are pruned keeps its rule. NULL is a run not
-- yet dispatched, which has no sandbox and so no content to keep in: the doors read its recorded policy, and a
-- run with none as allowed. A run from before the column gets its rule from the two UPDATEs below. Nullable
-- with no default, but the inline rule makes Postgres read every existing run once, and the whole file runs in
-- one transaction: the ALTER's exclusive lock on agent_runs is held through that read, the fill's scan of the
-- run.policy.resolve, run.policy.effective and run.policy.generation rows in audit_events, and both UPDATEs.
-- agent_runs already has its grants.
ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS data_out TEXT
    CHECK (data_out IN ('allowed', 'review', 'blocked'));

-- Filled from the policy rows retention has kept: each run gets the strictest rule among its run.policy.resolve
-- row (run.policy.effective in older releases) and its run.policy.generation rows. A resolve row with no rule
-- reads as allowed. A generation row with no rule (a Kubernetes egress edit's, or a revive of a run whose
-- resolve row was gone) says nothing and is skipped, so a run left with only such rows is not filled here. A
-- revive only raises the rule, so the strictest row is at least the rule in force.
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
 WHERE r.id = f.run_id AND r.data_out IS NULL AND f.rule IS NOT NULL;

-- A run still NULL has no policy row with a rule left. One that was dispatched may hold content whose rule is
-- unknown, so it is set to blocked rather than read as allowed. Dispatch writes the run.policy.resolve row
-- after it claims the run out of PENDING (state STARTING) and before the sandbox exists (sandbox_ref, set once
-- it does). A run still QUEUED, PENDING or STARTING with no sandbox has not reached that row: it stays NULL,
-- and its dispatch writes its rule (a previous release's, the resolve row). Every other run left NULL is set
-- to blocked, including one that failed or was killed before that row, which holds no content to lose.
UPDATE agent_runs
   SET data_out = 'blocked'
 WHERE data_out IS NULL
   AND NOT (state IN ('QUEUED', 'PENDING', 'STARTING') AND sandbox_ref = '');
