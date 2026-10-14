-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- A revocation cut is ordered against a credential by a logical sequence, not by comparing two
-- wall-clock readings: a step of the host clock between an issue and a cut can make the earlier
-- credential read as the later one. Every cut row takes its seq from this sequence; a credential
-- records the sequence's last value when it is issued (the session cookie's "rseq", rev_seq on an
-- API token, an SSH key, an attach ticket and a delegated token) and a cut with a greater seq ends it.
-- rev_seq 0 is a credential with no recorded order: a session cookie, attach ticket or delegated token
-- issued before this migration, or by a replica that predates it. It keeps the revoked_at comparison;
-- those expire within the session lifetime. API tokens and SSH keys can outlive any session, so the
-- rows that exist now are given an order below, once.
CREATE SEQUENCE IF NOT EXISTS oidc_revocation_seq;

ALTER TABLE oidc_session_revocations ADD COLUMN IF NOT EXISTS seq BIGINT NOT NULL DEFAULT 0;
ALTER TABLE oidc_session_cuts ADD COLUMN IF NOT EXISTS seq BIGINT NOT NULL DEFAULT 0;
ALTER TABLE api_tokens ADD COLUMN IF NOT EXISTS rev_seq BIGINT NOT NULL DEFAULT 0;
ALTER TABLE ssh_public_keys ADD COLUMN IF NOT EXISTS rev_seq BIGINT NOT NULL DEFAULT 0;
ALTER TABLE attach_tickets ADD COLUMN IF NOT EXISTS rev_seq BIGINT NOT NULL DEFAULT 0;
ALTER TABLE delegated_tokens ADD COLUMN IF NOT EXISTS rev_seq BIGINT NOT NULL DEFAULT 0;

-- A per-person token revocation reads the tokens a name owns under either form (principal = $1 OR
-- lower(email) = lower($1)); with api_tokens_principal_idx (0045) this index lets it avoid reading the
-- whole table once per member of a group removal.
CREATE INDEX IF NOT EXISTS api_tokens_lower_email_idx ON api_tokens (lower(email));

-- Reserves 1: it sits below every cut seq given here, which is what a credential created before any
-- cut records, and it leaves last_value a real position (at least 1), so a credential issued from here
-- on never records 0 or a value a later cut also draws.
SELECT nextval('oidc_revocation_seq');

-- Existing cuts get a seq in revoked_at order. They still end every credential they ended before: a
-- credential without a rev_seq is compared on revoked_at, and one with a rev_seq is issued after this
-- point, above every seq given here.
UPDATE oidc_session_revocations r SET seq = o.n
  FROM (SELECT sub, nextval('oidc_revocation_seq') AS n
          FROM (SELECT sub FROM oidc_session_revocations WHERE seq = 0 ORDER BY revoked_at, sub) q) o
 WHERE r.sub = o.sub AND r.seq = 0;
UPDATE oidc_session_cuts r SET seq = o.n
  FROM (SELECT sub, nextval('oidc_revocation_seq') AS n
          FROM (SELECT sub FROM oidc_session_cuts WHERE seq = 0 ORDER BY cut_at, sub) q) o
 WHERE r.sub = o.sub AND r.seq = 0;

-- Freezes the order of the API tokens and SSH keys that exist now. A token or key is ended by a cut
-- recorded at or after its created_at (the comparison this replaces), so it takes the seq of the latest
-- cut strictly before it, or the reserved 1: a cut after it has a greater seq, a cut before it does
-- not. Cookies are not read against oidc_session_cuts for these, so only oidc_session_revocations
-- counts. Rows already ordered are left alone.
--
-- A service identity's token (principal service:<name>, no email) is read against its sponsor's cuts,
-- its own principal's (a queued start) and the global one, so it is ordered against all three; the
-- human-token update below then skips it.
UPDATE api_tokens t SET rev_seq = COALESCE((SELECT max(r.seq) FROM oidc_session_revocations r
        WHERE (r.sub = s.sponsor OR r.sub = t.principal OR r.sub = '')
          AND r.revoked_at < t.created_at), 1)
  FROM service_identities s
 WHERE t.rev_seq = 0 AND t.principal = 'service:' || s.name;
UPDATE api_tokens t SET rev_seq = COALESCE((SELECT max(r.seq) FROM oidc_session_revocations r
        WHERE (r.sub = t.principal OR r.sub = '' OR lower(r.sub) = lower(t.email))
          AND r.revoked_at < t.created_at), 1)
 WHERE t.rev_seq = 0;
UPDATE ssh_public_keys t SET rev_seq = COALESCE((SELECT max(r.seq) FROM oidc_session_revocations r
        WHERE (r.sub = t.principal OR r.sub = '')
          AND r.revoked_at < t.created_at), 1)
 WHERE t.rev_seq = 0;

-- Draws the seq for every insert and update, so a writer that predates this migration, running
-- through a rolling upgrade, still orders its cut. Writers that know about it also take the advisory
-- lock (RevocationOrderLockClass) before the statement; this trigger is what makes the draw
-- impossible to forget.
--
-- Both functions run as their OWNER, the role that ran this migration and owns the sequence, as
-- audit_append does (0057, 0111): a split-role install's app role holds table and function privileges
-- (docs/OPERATIONS.md) and nothing on sequences, and nextval() or a read of last_value in an ordinary
-- function is checked against the caller. Neither widens what the caller may do: the trigger function
-- runs only as a trigger, on a write the caller's own table privileges allowed, and
-- oidc_revocation_position() returns the one number every issuer records anyway. The position function
-- keeps PostgreSQL's default EXECUTE for PUBLIC, so an app role created at any time can call it with no
-- grant of its own. search_path is pinned to the schema the tables are in, then pg_temp last, so no
-- caller's schema can shadow the sequence.
DO $mig$
DECLARE
    ns text;
BEGIN
    SELECT n.nspname INTO ns
      FROM pg_catalog.pg_class c
      JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
     WHERE c.oid = 'oidc_session_revocations'::regclass;

    EXECUTE format($fn$
CREATE OR REPLACE FUNCTION %1$I.oidc_revocation_stamp() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, %1$I, pg_temp
AS $body$
BEGIN
    NEW.seq := nextval('oidc_revocation_seq');
    RETURN NEW;
END;
$body$
$fn$, ns);

    -- The position a credential issued now records (store.ReadRevocationSeq).
    EXECUTE format($fn$
CREATE OR REPLACE FUNCTION %1$I.oidc_revocation_position() RETURNS bigint
LANGUAGE sql
SECURITY DEFINER
SET search_path = pg_catalog, %1$I, pg_temp
AS $body$ SELECT last_value FROM oidc_revocation_seq $body$
$fn$, ns);
END
$mig$;

DROP TRIGGER IF EXISTS oidc_session_revocations_stamp ON oidc_session_revocations;
CREATE TRIGGER oidc_session_revocations_stamp BEFORE INSERT OR UPDATE ON oidc_session_revocations
    FOR EACH ROW EXECUTE FUNCTION oidc_revocation_stamp();
DROP TRIGGER IF EXISTS oidc_session_cuts_stamp ON oidc_session_cuts;
CREATE TRIGGER oidc_session_cuts_stamp BEFORE INSERT OR UPDATE ON oidc_session_cuts
    FOR EACH ROW EXECUTE FUNCTION oidc_revocation_stamp();
