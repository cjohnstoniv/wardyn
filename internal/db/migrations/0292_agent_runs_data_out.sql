-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The run's data_out rule (allowed, review or blocked), the one every data_out door reads. Dispatch writes it
-- just before the run.policy.resolve row that records the policy, and a revive whose seal makes the rule
-- stricter raises it before its policy generation is stored. It is never lowered. Audit retention prunes the
-- audit rows and never this column, so a run whose policy rows are pruned keeps its rule. NULL is a run not
-- yet dispatched, or one from before the column whose policy rows were pruned before this migration: the doors
-- then read its recorded policy, and a run with none reads as blocked. Nullable with no default, but the
-- inline rule makes Postgres read every existing run once, and the whole file runs in one transaction: the
-- ALTER's exclusive lock on agent_runs is held through that read, the fill's scan of the run.policy.resolve,
-- run.policy.effective and run.policy.generation rows in audit_events, and the UPDATE of every run it fills.
-- agent_runs already has its grants.
ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS data_out TEXT
    CHECK (data_out IN ('allowed', 'review', 'blocked'));

-- Filled from the policy rows retention has kept: each run gets the strictest rule among its run.policy.resolve
-- row (run.policy.effective in older releases) and its run.policy.generation rows. A resolve row with no rule
-- reads as allowed. A generation row with no rule (a Kubernetes egress edit's, or a revive of a run whose
-- resolve row was gone) says nothing and is skipped, so a run left with only such rows stays NULL and reads as
-- blocked. A revive only raises the rule, so the strictest row is at least the rule in force.
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
