-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- principal_identities.first_login_at: when a person first signed in through this identity, kept apart from
-- when the identity row was made. A SCIM user is provisioned as a row with no principal, often long before
-- their first sign-in binds it, so created_at is the provisioning time and is no evidence of a sign-in.
-- The sign-in path writes it once and never changes it.
--
-- A row made by a sign-in has created_at as its first sign-in, which the backfill below records. A row any
-- SCIM write has touched is left NULL: its first sign-in cannot be told from its provisioning, and the people
-- directory shows an unknown date for it, not a wrong one. A portal person's first use is their row's
-- created_at, which the directory reads directly. The column holds a time and no secret.
ALTER TABLE principal_identities ADD COLUMN IF NOT EXISTS first_login_at TIMESTAMPTZ;

UPDATE principal_identities
   SET first_login_at = created_at
 WHERE first_login_at IS NULL
   AND last_login_at IS NOT NULL
   AND scim_external_id = ''
   AND scim_user_name = ''
   AND NOT starts_with(issuer, 'delegate:person:');
