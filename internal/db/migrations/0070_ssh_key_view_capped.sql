-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- A key an admin registers while their session is in the user view (the
-- member-mode clamp) is stored CAPPED: it is a member key for good. The OIDC
-- login re-stamp (store.RefreshSSHKeyRoles) leaves a capped row's role alone,
-- and sshAuth refuses the admin override for a capped row outright, so a key
-- made in the user view never quietly becomes an admin credential that
-- outlives the view that made it.
--
-- DEFAULT false: every existing key was registered outside the user view (the
-- key door answered 409 there until now), so none of them is capped.
--
-- The CHECK makes the cap structural rather than a convention of the two Go
-- call sites: no write path, including a hand-run UPDATE, can give a capped
-- row the admin role the gateway's override reads.
ALTER TABLE ssh_public_keys ADD COLUMN IF NOT EXISTS capped BOOLEAN NOT NULL DEFAULT false;

ALTER TABLE ssh_public_keys DROP CONSTRAINT IF EXISTS ssh_public_keys_capped_member;

ALTER TABLE ssh_public_keys
    ADD CONSTRAINT ssh_public_keys_capped_member
    CHECK (NOT capped OR role = 'member');
