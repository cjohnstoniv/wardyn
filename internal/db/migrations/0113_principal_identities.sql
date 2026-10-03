-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- One row per human identity Wardyn has seen sign in, on any issuer, so a leaver
-- has an identity row to deactivate even when nobody ever set them up in `people`
-- (most Entra sign-ins have no people row: Entra's `sub` is pairwise).
--
-- An Entra identity is (issuer, tenant_id, object_id); any other issuer has an
-- empty tenant_id and object_id and is keyed by (issuer, principal). principal is
-- NULL only on a row written before the person's first sign-in, which the first
-- sign-in binds. The binding columns (principal, issuer, tenant_id, object_id)
-- never change once set: every store write that touches one is WHERE principal IS
-- NULL. No trigger enforces it, because a trigger function would make this
-- migration need ownership of the schema for no gain over the store guard.
--
-- deactivated_at, authority_epoch, purge_after and purged_at are written by the
-- leaver work that follows; they are here so that work does not ALTER this table.
-- authority_epoch only ever rises. The rows hold no secret.
CREATE TABLE IF NOT EXISTS principal_identities (
    id               UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    principal        TEXT,
    issuer           TEXT        NOT NULL,
    tenant_id        TEXT        NOT NULL DEFAULT '',
    object_id        TEXT        NOT NULL DEFAULT '',
    email_lower      TEXT        NOT NULL DEFAULT '',
    scim_external_id TEXT        NOT NULL DEFAULT '',
    scim_user_name   TEXT        NOT NULL DEFAULT '',
    deactivated_at   TIMESTAMPTZ,
    authority_epoch  BIGINT      NOT NULL DEFAULT 0,
    purge_after      TIMESTAMPTZ,
    purged_at        TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_login_at    TIMESTAMPTZ,
    UNIQUE (principal, issuer, tenant_id, object_id)
);

CREATE UNIQUE INDEX IF NOT EXISTS principal_identities_object_key
    ON principal_identities (issuer, tenant_id, object_id) WHERE object_id <> '';
CREATE UNIQUE INDEX IF NOT EXISTS principal_identities_principal_key
    ON principal_identities (issuer, principal) WHERE principal IS NOT NULL;

-- Every email (and, later, SCIM userName) an identity has been seen under, kept
-- after it changes so a later removal can still find the identity by an old
-- address. source says which door recorded it.
CREATE TABLE IF NOT EXISTS principal_identity_aliases (
    identity_id UUID        NOT NULL REFERENCES principal_identities (id) ON DELETE CASCADE,
    kind        TEXT        NOT NULL CHECK (kind IN ('email', 'user_name')),
    value_lower TEXT        NOT NULL,
    source      TEXT        NOT NULL CHECK (source IN ('login', 'scim')),
    first_seen  TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (identity_id, kind, value_lower)
);

CREATE INDEX IF NOT EXISTS principal_identity_aliases_value_idx
    ON principal_identity_aliases (kind, value_lower);
