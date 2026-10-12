-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Launch intents (0.9): the setup a person launched a run with, kept as a
-- wardyn/v1 kind Run document so Run again can open it at Review. Written in
-- the run row's own transaction, readable only by the run's creator, and
-- deleted with the run (cascade) or by erasure (run_tasks, components and
-- run_setups). A document holds no title, description, secret value or run
-- state: the one template door refused it otherwise and the run keeps none.

CREATE TABLE IF NOT EXISTS run_launch_intents (
  run_id     uuid PRIMARY KEY REFERENCES agent_runs(id) ON DELETE CASCADE,
  created_by text NOT NULL,
  document   jsonb NOT NULL,
  created_at timestamptz NOT NULL
);

CREATE INDEX IF NOT EXISTS run_launch_intents_by_creator ON run_launch_intents (created_by, created_at DESC);
