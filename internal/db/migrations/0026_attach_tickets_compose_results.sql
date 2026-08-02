-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Two short-lived control-plane handoff tables, moved out of process memory.
--
-- Both were in-process singletons — Server.attachTix (a mutex-guarded map) and
-- a compose-result sync.Map — so a ticket minted on one wardynd was unknown to
-- another, a proposal uploaded to one was never found by the launcher waiting
-- on the other, and a restart silently dropped both. Persisted here, both work
-- across processes AND survive a crash. Consume-once stays EXACT because each
-- consumer is a single DELETE ... RETURNING: two racing redemptions, on one
-- control plane or two, can only have one return a row.
--
-- Neither table gets a background sweeper: rows are deleted on use, and the
-- stragglers (a ticket nobody redeemed, a proposal nobody took) are swept
-- opportunistically by the next insert. Both are a handful of rows.

CREATE TABLE IF NOT EXISTS attach_tickets (
    token      TEXT        PRIMARY KEY,
    run_id     UUID        NOT NULL,
    actor_type TEXT        NOT NULL,
    principal  TEXT        NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL
);

-- payload is claude's RAW stdout wrapper, byte-for-byte. BYTEA, not jsonb/text:
-- this layer is deliberately facts-out and never parses the upload
-- (internal/api/composeresult.go), and the wire can deliver a nonzero-exit
-- wrapper, an empty body, or a binary crash tail — text/jsonb would reject
-- NUL/non-UTF-8 bytes and turn a clear downstream parse error into a 500 on
-- upload (the old in-process map accepted any bytes; this preserves that).
CREATE TABLE IF NOT EXISTS compose_results (
    run_id     UUID        PRIMARY KEY,
    payload    BYTEA       NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
