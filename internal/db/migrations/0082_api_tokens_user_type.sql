-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- api_tokens.user_type: the user type a token was minted under (0.8, user-types
-- design section 2.5), stamped beside role at mint and re-stamped with it at
-- the holder's next sign-in (store.RefreshAPITokenIdentity).
--
-- No foreign key: it is a snapshot, like role. A type a live token still
-- carries is kept from deletion by the user-type delete guard instead, which
-- names the count.
--
-- Every existing token is backfilled 'standard', the type the old member tier
-- always meant; it learns its holder's real type at their next sign-in. The
-- column keeps its DEFAULT so an insert that omits it still stamps
-- 'standard' rather than failing; the Go mint and re-stamp paths always name
-- the column explicitly, and the CHECK refuses an empty stamp, so only an
-- omitted column, never an empty one, can produce a token with no real type.
ALTER TABLE api_tokens ADD COLUMN IF NOT EXISTS user_type TEXT NOT NULL DEFAULT 'standard';

ALTER TABLE api_tokens DROP CONSTRAINT IF EXISTS api_tokens_user_type_check;

ALTER TABLE api_tokens ADD CONSTRAINT api_tokens_user_type_check CHECK (user_type <> '');
