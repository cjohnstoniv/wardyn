-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- What a runner's state page says about a kill or end of a run that it refused because it could not see it
-- finish (a macOS VM whose helper outlived its kill), and makes again on a later session: one row per run whose
-- runner reported such a hold. cleared_at is NULL while the runner holds the run, and the time it says that kill
-- or end finished once it reports so. Only that report clears it; a state page without one leaves the row as it
-- is. A later hold of the same run opens the row again. kind is the action held ('kill' or 'end'); since and
-- cleared_at are the runner's own times. Never a secret.
--
-- Split-role installs grant the app role SELECT, INSERT, UPDATE on it (CHANGELOG, Before you upgrade).
CREATE TABLE IF NOT EXISTS run_teardown_holds (
    run_id      UUID PRIMARY KEY REFERENCES agent_runs(id) ON DELETE CASCADE,
    runner_id   UUID NOT NULL,
    kind        TEXT NOT NULL CHECK (kind IN ('kill', 'end')),
    since       TIMESTAMPTZ NOT NULL,
    cleared_at  TIMESTAMPTZ,
    observed_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
