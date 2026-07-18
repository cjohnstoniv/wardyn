-- The healthz-honesty fix records runner_target='none' for a control
-- plane started with -runner none (headless API-only; runs stay PENDING
-- forever) instead of the old hardcoded 'docker'. The original CHECK predates
-- that and rejected the honest value, so EVERY run insert failed under
-- -runner none (23514). Widen the constraint to the third honest target.
ALTER TABLE agent_runs DROP CONSTRAINT agent_runs_runner_target_check;
ALTER TABLE agent_runs ADD CONSTRAINT agent_runs_runner_target_check
    CHECK (runner_target IN ('docker', 'k8s', 'none'));
