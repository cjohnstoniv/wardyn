-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- A run's proxy starts and their policy readback (docs/operations/run-lifetime.md). Each start is
-- recorded before its proxy starts, under the generation and the fresh run token's jti it runs on.
-- expected_sha256 is the driver's hash of the bytes it handed the proxy, reported_sha256 the proxy's
-- own hash of what it loaded; the start is enforced only when both are in and equal. deadline is set
-- when the driver answers: the report must be in by then. A failed start's proxy stop is recorded on
-- it: contained_at once the stop is confirmed, containment_error while it is not (the reconcile sweep
-- retries it).
CREATE TABLE IF NOT EXISTS run_proxy_starts (
    run_id          UUID        NOT NULL REFERENCES agent_runs(id) ON DELETE CASCADE,
    generation      INTEGER     NOT NULL,
    jti             TEXT        NOT NULL,
    mode            TEXT        NOT NULL CHECK (mode IN ('expected', 'legacy')),
    started_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    deadline        TIMESTAMPTZ,
    expected_sha256 TEXT,
    reported_sha256 TEXT,
    state           TEXT        NOT NULL DEFAULT 'pending'
        CHECK (state IN ('pending', 'enforced', 'unconfirmed', 'failed', 'abandoned', 'superseded')),
    confirmed_at    TIMESTAMPTZ,
    contained_at    TIMESTAMPTZ,
    containment_error TEXT,
    PRIMARY KEY (run_id, generation)
);
CREATE INDEX IF NOT EXISTS run_proxy_starts_overdue_idx
    ON run_proxy_starts (deadline) WHERE state = 'pending' AND deadline IS NOT NULL;
CREATE INDEX IF NOT EXISTS run_proxy_starts_uncontained_idx
    ON run_proxy_starts (run_id) WHERE state = 'failed' AND contained_at IS NULL;

-- The address the run's agent is pinned to for its proxy, from the driver's create answer. NULL for
-- a run started before it, or by an older driver or runner.
ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS proxy_addr TEXT;
