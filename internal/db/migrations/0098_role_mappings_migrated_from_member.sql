-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- role_mappings.migrated_from_member marks a console row 0074's rename
-- rewrote from role='member' (0.8, user-types design §4): after that rewrite
-- a row an org actually saved as Standard user is indistinguishable from one
-- the rename touched, and the People page cannot honestly show a "Migrated
-- from member" chip without a marker.
--
-- Backfill cutoff: the migration tracking table this codebase reserves for
-- Migrate()'s own bookkeeping is off limits here, so this cannot look up
-- 0074's own apply moment to find the exact rewrite. Falling back, per the
-- design's own allowance for "when that timestamp is not recorded", to
-- 0074's LANDING commit instead: 2026-09-23T22:20:09Z (UTC;
-- eda59c3fcff4bd052dc57a68a48b50cc61a70e2b, "renumber migration
-- 0073_user_tier_rename to 0074"). role_mappings_role_check already forbids
-- 'member' outright once 0074 applies, so nothing saved afterward can ever
-- be a false negative here; the one residual is a genuinely fresh
-- Standard-user row saved in the few hours around 0074's own landing, which
-- is marked migrated in error and only needs a type chosen for it once, the
-- same click a truly migrated row asks for.
ALTER TABLE role_mappings ADD COLUMN IF NOT EXISTS migrated_from_member BOOLEAN NOT NULL DEFAULT false;

UPDATE role_mappings
SET migrated_from_member = true
WHERE role = 'user' AND user_type = 'standard'
  AND created_at < '2026-09-23T22:20:09Z';
