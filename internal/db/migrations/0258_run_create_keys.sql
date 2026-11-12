-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- A POST /runs Idempotency-Key: one row per (principal, via, key hash), kept 24 hours. principal is the
-- caller, via the delegate that acted for it ('' for a direct request); only the key's SHA-256 is kept,
-- never the key. request_sha256 is the request the key was first used with. claim_token and claim_until
-- are the lease of the request that holds the key until it inserts its run. run_id is bound in the same
-- transaction that inserts the run, so a key never names a run that was not made and a run is never made
-- twice for one key. The response_* columns hold the answer the first caller got once the key names a
-- run: NULL until saved, or when the body passed the size cap. BYTEA, so a replay is byte-equal.
-- response_erased is set when erasing the person's run tasks cleared the answer: no later save refills it.
-- A split-role install grants the app role SELECT, INSERT, UPDATE, DELETE on it.
CREATE TABLE IF NOT EXISTS run_create_keys (
    principal         TEXT        NOT NULL,
    via               TEXT        NOT NULL DEFAULT '',
    key_sha256        TEXT        NOT NULL,
    request_sha256    TEXT        NOT NULL,
    claim_token       UUID        NOT NULL,
    claim_until       TIMESTAMPTZ NOT NULL,
    run_id            UUID        REFERENCES agent_runs (id) ON DELETE CASCADE,
    response_status   INTEGER,
    response_location TEXT,
    response_body     BYTEA,
    response_erased   BOOLEAN     NOT NULL DEFAULT false,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at        TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (principal, via, key_sha256)
);

CREATE INDEX IF NOT EXISTS run_create_keys_expires_idx ON run_create_keys (expires_at);
