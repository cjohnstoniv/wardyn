-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- runner_resident delivery (0.9.3): one obligation row per run whose
-- runner_resident values were handed to its runner. The row is written before
-- the first value is sent, under a short delivery lease, and closed once the
-- runner confirms the erase (erased_at) or the org gives up on a confirmation
-- (abandoned_at, with its runner.resident.erase audit row in the same
-- transaction). not_after bounds how long the runner may read a value.
--
--   delivery_holder, delivery_lease_until  the dispatch delivering the values;
--                   an erase claim waits until the lease is released or expired
--   erase_begun_at  set by the first erase claim; delivery refuses from then on
--   next_erase_at   the next claim time: 30 s after the first claim, doubling to 1 h
--
-- ON DELETE RESTRICT on both keys: no run is ever deleted and only an
-- unclaimed runner (which holds no run) is, so a future purge must close the
-- obligation first, and until then the constraint makes it fail loudly.
-- Split-role installs grant the app role SELECT, INSERT, UPDATE on the table.
CREATE TABLE IF NOT EXISTS runner_resident_runs (
    run_id               UUID PRIMARY KEY REFERENCES agent_runs (id) ON DELETE RESTRICT,
    runner_id            UUID NOT NULL REFERENCES runners (id) ON DELETE RESTRICT,
    not_after            TIMESTAMPTZ NOT NULL,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    delivery_holder      UUID,
    delivery_lease_until TIMESTAMPTZ,
    erase_begun_at       TIMESTAMPTZ,
    erased_at            TIMESTAMPTZ,
    erase_attempts       INT NOT NULL DEFAULT 0,
    next_erase_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    erase_last_reason    TEXT,
    abandoned_at         TIMESTAMPTZ,
    abandoned_reason     TEXT CHECK (abandoned_reason IN ('runner_revoked', 'runner_deleted', 'expired_unconfirmed'))
);
CREATE INDEX IF NOT EXISTS runner_resident_runs_due ON runner_resident_runs (runner_id, next_erase_at)
    WHERE erased_at IS NULL AND abandoned_at IS NULL;
