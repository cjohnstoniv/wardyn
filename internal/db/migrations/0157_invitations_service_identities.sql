-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Owner-redeemed invitations and service identities (0.9, #1508; design record
-- docs/design/0.9/invitations-service-identities.md). One file for the whole feature so
-- every slice builds on the same tables. api_tokens and agent_runs are not touched: no
-- column added, no row relabelled.

-- 1. The service: prefix becomes a reserved principal namespace. A name that any
-- principal-keyed table already holds could carry its grants, secrets or history into a
-- service identity, so the upgrade refuses, naming the first table it finds.
DO $mig$
DECLARE
    probe  text[];
    probes text[][] := ARRAY[
        ['principal_identities', 'principal', 'true'],
        ['people', 'principal', 'true'],
        ['api_tokens', 'principal', 'true'],
        ['ssh_public_keys', 'principal', 'true'],
        ['agent_runs', 'created_by', 'true'],
        ['workspaces', 'owned_by', 'true'],
        ['secrets', 'owned_by', 'true'],
        ['capability_grants', 'subject', 'subject_type = ''user'''],
        ['governance_assignments', 'subject', 'subject_type = ''user''']
    ];
    n bigint;
BEGIN
    FOREACH probe SLICE 1 IN ARRAY probes LOOP
        EXECUTE format('SELECT count(*) FROM %I WHERE lower(%I) LIKE %L AND %s', probe[1], probe[2], 'service:%', probe[3]) INTO n;
        IF n > 0 THEN
            RAISE EXCEPTION 'migration 0157: % row(s) in %.% already use the reserved service: prefix; rename or remove them first',
                n, probe[1], probe[2];
        END IF;
    END LOOP;
END
$mig$;

-- 2. An invitation names the one person who may claim it and carries no secret: the
-- target's own signed-in session is the only authority. State is derived, never stored:
-- pending, claimed, expired or revoked. claimed_at and token_id are set together, once, by
-- the claim; token_id is metadata, never a credential.
CREATE TABLE IF NOT EXISTS person_invitations (
    id                UUID PRIMARY KEY,
    target            TEXT NOT NULL CHECK (target <> ''),
    created_by        TEXT NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at        TIMESTAMPTZ NOT NULL,
    token_name        TEXT NOT NULL DEFAULT '',
    token_ttl_seconds BIGINT CHECK (token_ttl_seconds IS NULL OR token_ttl_seconds >= 0),
    replaces_token_id UUID REFERENCES api_tokens (id),
    claimed_at        TIMESTAMPTZ,
    token_id          UUID REFERENCES api_tokens (id),
    revoked_at        TIMESTAMPTZ,
    revoked_by        TEXT,
    CHECK ((claimed_at IS NULL) = (token_id IS NULL)),
    CHECK ((revoked_at IS NULL) = (revoked_by IS NULL)),
    CHECK (claimed_at IS NULL OR revoked_at IS NULL)
);
CREATE INDEX IF NOT EXISTS person_invitations_target_idx ON person_invitations (target, created_at DESC);

-- 3. Service identities (docs/design/0.9/service-identities.md): schema only here; the
-- slices that write them come later. A name is never reused, so a retired row stays.
CREATE TABLE IF NOT EXISTS service_identities (
    name          TEXT PRIMARY KEY CHECK (name ~ '^[a-z][a-z0-9-]{2,62}$'),
    description   TEXT NOT NULL DEFAULT '' CHECK (char_length(description) <= 200),
    sponsor       TEXT NOT NULL CHECK (sponsor <> ''),
    sponsor_since TIMESTAMPTZ NOT NULL DEFAULT now(),
    user_type     TEXT NOT NULL DEFAULT 'standard' CHECK (user_type <> ''),
    created_by    TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    suspended_at  TIMESTAMPTZ,
    suspended_by  TEXT,
    retired_at    TIMESTAMPTZ,
    retired_by    TEXT,
    CHECK ((suspended_at IS NULL) = (suspended_by IS NULL)),
    CHECK ((retired_at IS NULL) = (retired_by IS NULL))
);

-- 4. An admin-minted token linked to the service identity that replaces it.
CREATE TABLE IF NOT EXISTS api_token_moves (
    token_id         UUID PRIMARY KEY REFERENCES api_tokens (id),
    service_identity TEXT NOT NULL REFERENCES service_identities (name),
    linked_by        TEXT NOT NULL,
    linked_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 5. A service identity is an audit actor of its own. The check on the partitioned
-- parent propagates to every partition. It takes ACCESS EXCLUSIVE on the parent and its
-- partitions for the length of the validating scan, about 0.2 s over twelve partitions of
-- 100k rows. The widened set admits every row already stored.
ALTER TABLE audit_events
    DROP CONSTRAINT IF EXISTS audit_events_actor_type_check,
    ADD CONSTRAINT audit_events_actor_type_check CHECK (actor_type IN ('human', 'agent', 'system', 'service'));
