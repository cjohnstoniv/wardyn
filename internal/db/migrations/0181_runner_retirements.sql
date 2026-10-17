-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Runner run retirement (0.9.3): the org retires a terminal runner run, so its
-- runner deletes the run's settled output and seals its spool.
--
-- runner_output_watermarks: one row per runner run dispatched from 0.9.3 on.
--   capture       the dispatch asked the runner to keep the agent's output
--   accepted_end  the end of the output the org accepted, committed before the
--                 runner is told; it only grows, and 0 is a real answer
-- No row: the run predates this table and is never retired.
--
-- runner_retirements: the binding snapshot of one retirement obligation, taken
-- when the org clears the run's ref (or from the live ref of a killed run).
--   state  kill (the containment kill is queued or deferred), retire (the kill
--          succeeded and the retire_run is queued), then settled, refused or
--          abandoned; a closed row is never reopened
--   successors  fresh-ID successors of deferred actions, both phases counted
--
-- ON DELETE RESTRICT on every key: no run is deleted, and only an unclaimed
-- runner (which holds no run) is. Split-role installs grant the app role
-- SELECT, INSERT, UPDATE on both tables.
CREATE TABLE IF NOT EXISTS runner_output_watermarks (
    run_id       UUID PRIMARY KEY REFERENCES agent_runs (id) ON DELETE RESTRICT,
    capture      BOOLEAN NOT NULL,
    accepted_end BIGINT NOT NULL DEFAULT 0 CHECK (accepted_end >= 0),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS runner_retirements (
    run_id        UUID PRIMARY KEY REFERENCES agent_runs (id) ON DELETE RESTRICT,
    runner_id     UUID NOT NULL REFERENCES runners (id) ON DELETE RESTRICT,
    obligation_id UUID NOT NULL UNIQUE,
    ref           TEXT NOT NULL CHECK (ref <> ''),
    generation    UUID,
    exec_id       TEXT NOT NULL DEFAULT '',
    state         TEXT NOT NULL DEFAULT 'kill' CHECK (state IN ('kill', 'retire', 'settled', 'refused', 'abandoned')),
    successors    INT NOT NULL DEFAULT 0 CHECK (successors >= 0),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    settled_at    TIMESTAMPTZ,
    reason        TEXT NOT NULL DEFAULT '',
    CHECK ((state IN ('kill', 'retire')) = (settled_at IS NULL))
);
CREATE INDEX IF NOT EXISTS runner_retirements_open ON runner_retirements (runner_id) WHERE state IN ('kill', 'retire');

-- An exit that reaches the org after the run ended is kept as the run's exit
-- receipt only: it never enters the exit work, acceptance or the tail.
ALTER TABLE runner_execution_exits ADD COLUMN IF NOT EXISTS receipt_only BOOLEAN NOT NULL DEFAULT false;

-- 'abandoned': a retirement obligation closed without a settlement (its
-- deferred successors ran out); its deferred actions stop fencing the run.
ALTER TABLE runner_pending_actions
    DROP CONSTRAINT IF EXISTS runner_pending_actions_settlement,
    ADD CONSTRAINT runner_pending_actions_settlement CHECK (
        settlement IN ('', 'revoked', 'chain', 'abandoned') AND (settled_at IS NULL) = (settlement = '')
        AND (settlement = '' OR binding_version = 1)
        AND (settlement = 'chain') = (settled_by IS NOT NULL)
    );
