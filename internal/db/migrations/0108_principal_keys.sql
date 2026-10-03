-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Per-subject keys. One 32-byte key per (owner, purpose, generation), wrapped
-- under its key domain's key-encryption key through the kek.KEK seam, so the
-- local key, Vault Transit and Azure Key Vault all work unchanged. A purpose
-- is what the key seals: 'cred' (the owner's credential rows and the run
-- masking copies) or 'audit-seal' (sealed audit fields).
--
--   * One live generation per (owner, purpose): the partial unique index.
--   * Destroy is a tombstone, never a DELETE: destroyed_at is set and
--     wrapped_key cleared on every generation, so a version number is never
--     reused and the app role needs no DELETE on this table.
--   * wrapped_key is NULL exactly while destroyed_at is set.
--   * owner is never '' (the operator namespace holds the boot keys, which are
--     not subject keys).
--   * domain is 'default' (the deployment's credential KEK) until key domains
--     exist; kek_id names the key that wrapped the row, as secrets.kek_id does.
--
-- Split-role installs grant the app role SELECT, INSERT, UPDATE on this table.
CREATE TABLE IF NOT EXISTS principal_keys (
    owner        TEXT        NOT NULL CHECK (owner <> ''),
    purpose      TEXT        NOT NULL CHECK (purpose IN ('cred', 'audit-seal')),
    version      INT         NOT NULL CHECK (version >= 1),
    domain       TEXT        NOT NULL,
    kek_id       TEXT        NOT NULL,
    wrapped_key  BYTEA,
    destroyed_at TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (owner, purpose, version),
    CONSTRAINT principal_keys_wrapped_while_live CHECK ((wrapped_key IS NULL) = (destroyed_at IS NOT NULL))
);

CREATE UNIQUE INDEX IF NOT EXISTS principal_keys_one_live
    ON principal_keys (owner, purpose) WHERE destroyed_at IS NULL;
