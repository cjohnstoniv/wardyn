-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Inbound federation (#3787): a token from an organization's own identity provider is exchanged for a short-lived
-- inbound token of one service identity. Off until an admin turns it on. A trust matches the token's issuer,
-- audience and subject by exact bytes and names the one identity it becomes. inbound_trust_tokens links each
-- exchanged token to its trust, so a trust delete or the switch turned off revokes exactly those tokens. An exchanged
-- token's api_tokens row is deleted by a later exchange under the same trust, at the earliest a day after it expires,
-- or when its trust is removed; its inbound.exchange audit row is the record. A removed trust's revoked, unexpired
-- rows are kept, unlinked. A trust records the revocation sequence it was last confirmed at: a later revoke of its
-- sponsor stops it until the sponsor confirms it again. created_by, confirmed_by and updated_by name a person, and
-- erasing that person clears them to ''. No provider token and no Wardyn token is stored here.
CREATE TABLE IF NOT EXISTS inbound_federation (
    id         BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id),
    enabled    BOOLEAN NOT NULL DEFAULT FALSE,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by TEXT NOT NULL DEFAULT ''
);
INSERT INTO inbound_federation (id) VALUES (TRUE) ON CONFLICT DO NOTHING;

CREATE TABLE IF NOT EXISTS inbound_trusts (
    id                    UUID PRIMARY KEY,
    name                  TEXT NOT NULL UNIQUE CHECK (name ~ '^[a-z][a-z0-9-]{2,62}$'),
    issuer                TEXT NOT NULL CHECK (issuer <> '' AND char_length(issuer) <= 2048),
    internal_issuer       TEXT NOT NULL DEFAULT '',
    audience              TEXT NOT NULL CHECK (audience <> '' AND char_length(audience) <= 512),
    subject               TEXT NOT NULL CHECK (subject <> '' AND char_length(subject) <= 512),
    required_claim        TEXT NOT NULL DEFAULT '',
    required_claim_value  TEXT NOT NULL DEFAULT '',
    service_name          TEXT NOT NULL REFERENCES service_identities(name),
    token_ttl_seconds     INT NOT NULL DEFAULT 900 CHECK (token_ttl_seconds BETWEEN 60 AND 3600),
    max_token_age_seconds INT NOT NULL DEFAULT 300 CHECK (max_token_age_seconds BETWEEN 60 AND 3600),
    max_live              INT NOT NULL DEFAULT 100 CHECK (max_live BETWEEN 1 AND 1000),
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by            TEXT NOT NULL,
    confirmed_at          TIMESTAMPTZ NOT NULL,
    confirmed_seq         BIGINT NOT NULL DEFAULT 0 CHECK (confirmed_seq >= 0),
    confirmed_by          TEXT NOT NULL,
    UNIQUE (issuer, audience, subject),
    CHECK ((required_claim = '') = (required_claim_value = ''))
);
CREATE INDEX IF NOT EXISTS inbound_trusts_service ON inbound_trusts (service_name);

CREATE TABLE IF NOT EXISTS inbound_trust_tokens (
    token_id   UUID PRIMARY KEY REFERENCES api_tokens(id) ON DELETE CASCADE,
    trust_id   UUID NOT NULL REFERENCES inbound_trusts(id) ON DELETE CASCADE,
    expires_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS inbound_trust_tokens_trust_exp ON inbound_trust_tokens (trust_id, expires_at);

-- The app role is granted the new tables here, found from the catalog and never by name: every role but the owner
-- that holds UPDATE on api_tokens, which every token revoke uses. The exchange's FOR SHARE on the switch and a trust,
-- and the switch write's FOR UPDATE on the switch and on every trust, a sponsor change's and a delete's FOR UPDATE,
-- need UPDATE on the table they lock; a confirm and an erasure update a trust; trusts and links are deleted.
DO $mig$
DECLARE
    r record;
BEGIN
    FOR r IN
        SELECT DISTINCT ro.rolname::text AS rolname
          FROM pg_catalog.pg_class c
          CROSS JOIN LATERAL pg_catalog.aclexplode(c.relacl) a
          JOIN pg_catalog.pg_roles ro ON ro.oid = a.grantee
         WHERE c.oid = 'api_tokens'::regclass
           AND a.privilege_type = 'UPDATE' AND a.grantee <> c.relowner
    LOOP
        EXECUTE format('GRANT SELECT, UPDATE ON inbound_federation TO %I', r.rolname);
        EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON inbound_trusts TO %I', r.rolname);
        EXECUTE format('GRANT SELECT, INSERT, DELETE ON inbound_trust_tokens TO %I', r.rolname);
    END LOOP;
END
$mig$;
