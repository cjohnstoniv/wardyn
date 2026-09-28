-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Per-principal secrets (member BYOK). Until now `secrets` was ONE global
-- namespace keyed by name alone (0001_init.sql) -- a member could never hold
-- their own "anthropic-api-key" alongside the operator's. owned_by is the
-- same dual-key identity a capability_grants `user` subject and
-- workspaces.owned_by (0048) carry: the lowercased OIDC sub (or email) of the
-- member who owns the row.
--
-- '' (the DEFAULT) means OPERATOR-OWNED: every secret that exists today, and
-- every secret an admin writes from here on (secretOwnerFromRequest returns
-- "" for an operator), keeps behaving exactly as it does now -- resolved by
-- every run that is not itself member-owned. Same back-compat shape as
-- workspaces.owned_by and capability_enforcement's "absent row = not
-- enforced": a pre-0.7 upgrade changes nothing until a member writes their
-- first owned secret.
--
-- The primary key moves from (name) to (owned_by, name) -- keeping name alone
-- would forbid two members each holding their own "anthropic-api-key", which
-- is the whole finding this migration closes.
ALTER TABLE secrets ADD COLUMN IF NOT EXISTS owned_by TEXT NOT NULL DEFAULT '';
ALTER TABLE secrets DROP CONSTRAINT IF EXISTS secrets_pkey;
ALTER TABLE secrets ADD PRIMARY KEY (owned_by, name);
