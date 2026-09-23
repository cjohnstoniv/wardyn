-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The non-admin tier is renamed "member" -> "user" (0.8, user-types design
-- section 4). Every stored copy of the old word is rewritten here, so no
-- column ever holds a tier value oidc.Roles does not name.
--
-- role_mappings (the People page rows): 'member' rows become role 'user' on
-- the built-in 'standard' type, the type the old tier always meant. The new
-- user_type column names the row's type. ON DELETE RESTRICT is what keeps a
-- type a saved row still names from being deleted.
--
-- api_tokens, ssh_public_keys, attach_tickets: role snapshots taken at mint
-- or registration. The first two also move their fail-closed column default
-- ('member' since 0045 and 0043). Only admin-vs-not is read from the last two,
-- and neither carries a CHECK (0034, 0043).

ALTER TABLE role_mappings DROP CONSTRAINT IF EXISTS role_mappings_role_check;

ALTER TABLE role_mappings
    ADD COLUMN IF NOT EXISTS user_type TEXT REFERENCES user_types (id) ON DELETE RESTRICT;

UPDATE role_mappings SET role = 'user', user_type = 'standard' WHERE role = 'member';

ALTER TABLE role_mappings
    ADD CONSTRAINT role_mappings_role_check
    CHECK (role IN ('admin', 'security_admin', 'user'));

ALTER TABLE api_tokens DROP CONSTRAINT IF EXISTS api_tokens_role_check;

UPDATE api_tokens SET role = 'user' WHERE role = 'member';

ALTER TABLE api_tokens ALTER COLUMN role SET DEFAULT 'user';

ALTER TABLE api_tokens
    ADD CONSTRAINT api_tokens_role_check
    CHECK (role IN ('admin', 'security_admin', 'user'));

UPDATE ssh_public_keys SET role = 'user' WHERE role = 'member';

ALTER TABLE ssh_public_keys ALTER COLUMN role SET DEFAULT 'user';

UPDATE attach_tickets SET role = 'user' WHERE role = 'member';
