-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The groups an identity provider's SCIM connector tells Wardyn about, and who it says is in them. They
-- exist for one purpose: when the provider removes a person from a group, their sessions are cut and the
-- API tokens whose login-time group snapshot holds that group are revoked. No authorisation decision reads
-- either table; a group created or a member added grants nothing.
--
-- external_id is the provider's own id for the group (Entra's group object id), the value a token's group
-- snapshot carries lower-cased, so a removal matches on lower(btrim(external_id)) and never on
-- display_name. A group has one row per external id however it is cased.
CREATE TABLE IF NOT EXISTS scim_groups (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    external_id  TEXT        NOT NULL,
    display_name TEXT        NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS scim_groups_external_id_key ON scim_groups (lower(btrim(external_id)));

CREATE TABLE IF NOT EXISTS scim_group_members (
    group_id    UUID        NOT NULL REFERENCES scim_groups (id) ON DELETE CASCADE,
    identity_id UUID        NOT NULL REFERENCES principal_identities (id) ON DELETE CASCADE,
    added_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (group_id, identity_id)
);
CREATE INDEX IF NOT EXISTS scim_group_members_identity_idx ON scim_group_members (identity_id);

-- A cutoff that ends browser sessions only. oidc_session_revocations (0049) is read by every credential
-- lane, so a cutoff there also ends the person's API tokens and SSH keys; a mover keeps the tokens whose
-- group snapshot does not hold the group they left. A row here is read for a session cookie and for the
-- grants issued under one, never for an API token or an SSH key. One row per sub, like 0049.
CREATE TABLE IF NOT EXISTS oidc_session_cuts (
    sub    TEXT        PRIMARY KEY,
    cut_at TIMESTAMPTZ NOT NULL
);
