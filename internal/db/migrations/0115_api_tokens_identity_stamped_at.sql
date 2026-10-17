-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- When a token's role and group stamp was last set: at mint, and again at every sign-in of its
-- owner (OnLogin re-stamps the role, user type and groups). WARDYN_ROLE_STAMP_TTL, when set,
-- refuses a token whose stamp is older until its owner signs in again, the same bound
-- WARDYN_SSH_ROLE_TTL puts on an SSH key (0046).
--
-- Backfilled to created_at, which is the one time that is known: later re-stamps were not timed.
-- A token a login re-stamped since mint therefore looks older than it is, so turning the TTL on
-- asks its holder to sign in once. That fails closed. A NULL stamp, which no write path leaves,
-- reads as stale under the TTL for the same reason.
ALTER TABLE api_tokens ADD COLUMN IF NOT EXISTS identity_stamped_at TIMESTAMPTZ;

UPDATE api_tokens SET identity_stamped_at = created_at WHERE identity_stamped_at IS NULL;
