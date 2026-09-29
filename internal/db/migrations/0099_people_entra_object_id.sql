-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- A person set up on Microsoft Entra ID before their first sign-in (#1195) is
-- keyed by the tenant-stable object id, not the pairwise `sub`: a sign-in
-- attaches only when its issuer, `tid` and `oid` claims equal these three
-- exactly. Such a row's principal is 'entra:<tenant_id>:<object_id>'. All three
-- are '' on a row keyed by subject, which is every legacy row.
ALTER TABLE people
    ADD COLUMN IF NOT EXISTS issuer TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS tenant_id TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS object_id TEXT NOT NULL DEFAULT '';

-- All three or none: a partial key would match on fewer than three claims.
ALTER TABLE people DROP CONSTRAINT IF EXISTS people_object_key_whole;
ALTER TABLE people ADD CONSTRAINT people_object_key_whole CHECK (
    (issuer = '' AND tenant_id = '' AND object_id = '') OR
    (issuer <> '' AND tenant_id <> '' AND object_id <> ''));
