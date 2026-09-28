-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- role_mappings.migrated_from_member marks a console row 0074's rename
-- rewrote from role='member' (0.8, user-types design §4): after that rewrite
-- a row an org actually saved as Standard user is indistinguishable from one
-- the rename touched, and the People page cannot honestly show a "Migrated
-- from member" chip without a marker.
--
-- Backfill: mark every role='user' AND user_type='standard' row, unconditional
-- on created_at. This is exact, not a heuristic, for any install upgrading
-- from a release: 0074 is unreleased (no shipped tag contains it), so a
-- 0.7.x deployment applies 0074 and this migration in the SAME Migrate()
-- call, back to back, with no application traffic served in between --
-- nothing can write a fresh role='user' row into the gap because there is no
-- gap. And before 0074 runs, role_mappings_role_check (0053) allows only
-- ('admin','security_admin','member') -- 'user' cannot exist in the table at
-- all until 0074's own rewrite puts it there. So at the moment this migration
-- runs, on a real upgrade, every 'user'/'standard' row IS 0074's rewrite.
--
-- The one place this over-marks is a dev/lab database that applied 0074 on
-- its own, kept serving traffic, and only later picked up this migration in
-- a separate deploy -- a genuinely fresh Standard-user row saved in that
-- window is marked migrated in error, and needs a type chosen for it once,
-- the same click a truly migrated row asks for. This file cannot narrow that
-- further: it has no way to ask WHEN 0074 itself applied (the migration
-- tracking table is Migrate()'s own bookkeeping, off limits to any
-- migration), so "every 0.7.x install" is the exact case this backfill gets
-- right, and the dev/lab case above is the one it cannot.
ALTER TABLE role_mappings ADD COLUMN IF NOT EXISTS migrated_from_member BOOLEAN NOT NULL DEFAULT false;

UPDATE role_mappings
SET migrated_from_member = true
WHERE role = 'user' AND user_type = 'standard';
