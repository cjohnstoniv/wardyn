-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The harnesses a run was launched with, written once at create: the set the
-- sandbox image carries and the model provider each harness was granted. A child
-- table, not an agent_runs column (scanRun binds columns by position; see 0137).
-- Each row is also an AUTHORIZATION TOMBSTONE: renew, revive and a queued start
-- re-check the agent and model-provider doors every harness needed. No personal
-- data: harness ids and provider ids are configuration, so erasure clears nothing.
-- Split-role installs grant the app role SELECT, INSERT on run_harnesses.
CREATE TABLE IF NOT EXISTS run_harnesses (
    run_id            UUID     NOT NULL REFERENCES agent_runs(id) ON DELETE CASCADE,
    ordinal           SMALLINT NOT NULL CHECK (ordinal >= 0 AND ordinal < 16),
    harness           TEXT     NOT NULL CHECK (harness ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$'),
    model_provider_id TEXT     NOT NULL DEFAULT '',
    starts            BOOLEAN  NOT NULL DEFAULT false,
    PRIMARY KEY (run_id, ordinal),
    UNIQUE (run_id, harness)
);
CREATE UNIQUE INDEX IF NOT EXISTS run_harnesses_one_start ON run_harnesses (run_id) WHERE starts;
