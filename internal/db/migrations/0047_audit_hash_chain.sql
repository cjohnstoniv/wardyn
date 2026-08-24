-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Hash-chain the append-only audit log (TIER-C #15).
--
-- WHAT THIS BUYS: the 0001/0004 triggers stop UPDATE/DELETE/TRUNCATE through
-- Wardyn's own schema, and 0007 hardens that against the app role. None of them
-- bind a DB OWNER or superuser, who can DISABLE TRIGGER and rewrite a row. A
-- chain does not stop that either — it makes a SINGLE rewritten or spliced-out
-- row DETECTABLE, because every row commits to its predecessor:
--
--     row_hash = SHA-256( prev_hash || canonical(immutable row fields) )
--
-- Tamper-EVIDENCE, not tamper-proofness: an actor who can rewrite one row can
-- usually rewrite the whole tail and re-chain it. What defeats that is the head
-- hash already shipped OFF-BOX on the audit sink stream (a SIEM that recorded
-- head H and later sees a chain that no longer contains H has proof of a
-- rewrite/truncation). Signed receipts are deliberately NOT in scope here.
--
-- LEGACY ROWS: every row that existed before this migration keeps NULL
-- prev_hash/row_hash. We do NOT backfill: a backfill computed by the same
-- process that could have tampered proves nothing, and it would rewrite the
-- append-only table. The chain therefore starts at the FIRST row inserted after
-- this migration, whose prev_hash is NULL (genesis) — so a NULL prev_hash means
-- either "legacy" (row_hash also NULL) or "genesis" (row_hash set).

ALTER TABLE audit_events ADD COLUMN IF NOT EXISTS prev_hash TEXT;
ALTER TABLE audit_events ADD COLUMN IF NOT EXISTS row_hash  TEXT;

-- Head lookup is one index probe even when a long legacy (NULL-hash) prefix
-- sits above the chain — without this the FIRST post-migration insert scans
-- every legacy row backwards looking for a row_hash it will not find.
CREATE INDEX IF NOT EXISTS audit_events_chain_head_idx
    ON audit_events (seq DESC) WHERE row_hash IS NOT NULL;

-- The ONE canonical serialization. Both the insert trigger below and the
-- operator-invoked verify sweep (store.PG.VerifyAuditChain) call this exact
-- function, so there is no second implementation that can disagree with the
-- first about what a row hashes to.
--
-- Encoding choices, all picked to be session- and locale-INDEPENDENT (a verify
-- run from a psql with a different TimeZone/DateStyle must reproduce the same
-- digest):
--   * time  -> microseconds since epoch as an integer. That is exactly
--     Postgres's own storage granularity, so a value round-trips bit-for-bit;
--     to_char/::text would drag DateStyle and lc_time in.
--   * data  -> embedded as jsonb INSIDE the array, so what is hashed is the
--     jsonb-normalized form the table actually stores, not the caller's
--     whitespace/key order. Hashing the pre-INSERT bytes would fail every
--     verify, because jsonb re-sorts keys on the way in.
--   * the field list is wrapped in jsonb_build_array so each element is JSON
--     -escaped: no separator character can be smuggled from one field into the
--     next to forge a colliding row.
--   * seq is NOT hashed (it is assigned by the identity default, which the
--     BEFORE trigger cannot see for the SELECT form). Position is bound by the
--     prev_hash link instead.
-- A SQL NULL data and a jsonb 'null' data hash identically; both mean "no
-- payload" and neither is distinguishable to a reader either.
CREATE OR REPLACE FUNCTION audit_row_hash(
    prev          TEXT,
    p_id          UUID,
    p_time        TIMESTAMPTZ,
    p_run_id      UUID,
    p_actor_type  TEXT,
    p_actor       TEXT,
    p_action      TEXT,
    p_target      TEXT,
    p_outcome     TEXT,
    p_source_ip   TEXT,
    p_data        JSONB
--
-- STABLE, not IMMUTABLE: everything in the body is immutable EXCEPT extract(),
-- which Postgres marks stable for the whole date_part family (some fields of a
-- timestamptz do depend on the session TimeZone — 'epoch' does not). Labelling
-- it immutable would be a claim the body cannot honestly make; nothing here
-- needs an index on the expression, so stable costs nothing.
) RETURNS TEXT LANGUAGE sql STABLE AS $$
    SELECT encode(sha256(convert_to(
        coalesce(prev, '') ||
        jsonb_build_array(
            p_id::text,
            (extract(epoch FROM p_time) * 1000000)::bigint::text,
            coalesce(p_run_id::text, ''),
            p_actor_type, p_actor, p_action, p_target, p_outcome, p_source_ip,
            p_data
        )::text, 'UTF8')), 'hex')
$$;

-- Chain the row at INSERT time. A BEFORE INSERT trigger (rather than Go-side
-- computation) is what makes the chain unforgeable BY THE CALLER: prev_hash and
-- row_hash are overwritten unconditionally, so a client that supplies its own
-- values cannot choose them, and the second in-tree insert path (the broker's
-- in-tx credential.mint write, which cannot import the store helper) is chained
-- without knowing this exists.
--
-- CONCURRENCY: this trigger does NOT take the serializing lock. It cannot — by
-- the time it runs, the identity default has already handed NEW its seq, so two
-- racing writers could take a lock here in the opposite order to their seq
-- allocation and leave chain order and seq order inverted, which is exactly what
-- the verify sweep would then report as tampering. The lock is therefore taken
-- by the CALLER, before the INSERT statement, on the same transaction:
-- pg_advisory_xact_lock(db.AuditChainLockKey) in store.InsertAuditEvent and in
-- the broker's insertAuditEventTx. Held to commit, so the next writer's head
-- read sees a committed row.
CREATE OR REPLACE FUNCTION audit_events_chain() RETURNS trigger AS $$
DECLARE
    head TEXT;
BEGIN
    SELECT row_hash INTO head
      FROM audit_events
     WHERE row_hash IS NOT NULL
     ORDER BY seq DESC
     LIMIT 1;
    NEW.prev_hash := head;
    -- "time" is quoted because it is a Postgres type name: unquoted it parses
    -- here, but quoting removes the question entirely.
    NEW.row_hash := audit_row_hash(head, NEW.id, NEW."time", NEW.run_id,
        NEW.actor_type, NEW.actor, NEW.action, NEW.target, NEW.outcome,
        NEW.source_ip, NEW.data);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS audit_events_chain ON audit_events;
CREATE TRIGGER audit_events_chain
    BEFORE INSERT ON audit_events
    FOR EACH ROW EXECUTE FUNCTION audit_events_chain();
