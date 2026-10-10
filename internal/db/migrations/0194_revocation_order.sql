-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- A revocation cut is ordered against a credential by a logical sequence, not by comparing two
-- wall-clock readings: a step of the host clock between an issue and a cut can make the earlier
-- credential read as the later one. Every cut row takes its seq from this sequence; a credential
-- records the sequence's last value when it is issued (the session cookie's "rseq", rev_seq on an
-- API token, an SSH key, an attach ticket and a delegated token) and a cut with a greater seq ends it. rev_seq 0 is a credential issued
-- before this migration, or by a replica that predates it: it keeps the revoked_at comparison.
CREATE SEQUENCE IF NOT EXISTS oidc_revocation_seq;

ALTER TABLE oidc_session_revocations ADD COLUMN IF NOT EXISTS seq BIGINT NOT NULL DEFAULT 0;
ALTER TABLE oidc_session_cuts ADD COLUMN IF NOT EXISTS seq BIGINT NOT NULL DEFAULT 0;
ALTER TABLE api_tokens ADD COLUMN IF NOT EXISTS rev_seq BIGINT NOT NULL DEFAULT 0;
ALTER TABLE ssh_public_keys ADD COLUMN IF NOT EXISTS rev_seq BIGINT NOT NULL DEFAULT 0;
ALTER TABLE attach_tickets ADD COLUMN IF NOT EXISTS rev_seq BIGINT NOT NULL DEFAULT 0;
ALTER TABLE delegated_tokens ADD COLUMN IF NOT EXISTS rev_seq BIGINT NOT NULL DEFAULT 0;

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

-- Draws the seq for every insert and update, so a writer that predates this migration, running
-- through a rolling upgrade, still orders its cut. Writers that know about it also take the advisory
-- lock (RevocationOrderLockClass) before the statement; this trigger is what makes the draw
-- impossible to forget.
CREATE OR REPLACE FUNCTION oidc_revocation_stamp() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    NEW.seq := nextval('oidc_revocation_seq');
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS oidc_session_revocations_stamp ON oidc_session_revocations;
CREATE TRIGGER oidc_session_revocations_stamp BEFORE INSERT OR UPDATE ON oidc_session_revocations
    FOR EACH ROW EXECUTE FUNCTION oidc_revocation_stamp();
DROP TRIGGER IF EXISTS oidc_session_cuts_stamp ON oidc_session_cuts;
CREATE TRIGGER oidc_session_cuts_stamp BEFORE INSERT OR UPDATE ON oidc_session_cuts
    FOR EACH ROW EXECUTE FUNCTION oidc_revocation_stamp();

-- Leaves the sequence called, so last_value is a real position (at least 1) and a credential issued
-- from here on never records 0.
SELECT nextval('oidc_revocation_seq');
