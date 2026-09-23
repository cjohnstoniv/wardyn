-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Widen role_mappings.role to the THIRD tier: security_admin (0.7 §B). 0051
-- created the column with an INLINE CHECK over exactly two values, written
-- when "admin"/"member" WAS the complete set; the tier core has since landed
-- oidc.RoleSecurityAdmin and widened oidc.ValidRole to accept it, so
-- POST /access/mappings now passes API validation with role=security_admin
-- and is refused by Postgres immediately after -- a 500 on a surface the
-- console offers. This closes that window. It is not an escalation (the
-- /access family is super-admin-only, and a mapped security_admin session is
-- strictly narrower than admin); it is a half-open surface.
--
-- WHY A NEW FILE and not an edit to 0051: 0051's CREATE TABLE is guarded by
-- IF NOT EXISTS, so editing it in place changes nothing on any deployment
-- that has already applied it -- the filename is the immutable migration
-- identity Migrate() records, and a re-run is a no-op. Widening an existing
-- constraint is always its own migration.
--
-- WHY DROP-THEN-ADD and not a second CHECK: two CHECKs on one column AND
-- together, so adding `role IN ('admin','security_admin','member')` beside
-- the old two-value one would leave security_admin refused by the older,
-- narrower constraint. The old one has to go.
--
-- The DROP names the constraint Postgres auto-generated for 0051's inline
-- single-column CHECK -- <table>_<column>_check -- and the ADD re-creates it
-- under that SAME name EXPLICITLY, so from here on the constraint is named
-- deterministically rather than by convention. IF EXISTS on the drop plus a
-- fixed name on the add makes the pair idempotent: re-running replaces the
-- constraint with an identical one instead of erroring on a duplicate name
-- (Postgres has no ADD CONSTRAINT IF NOT EXISTS).
--
-- BACKWARD-COMPATIBLE BY CONSTRUCTION: this WIDENS the accepted set, so no
-- existing row can violate the new constraint -- every stored role is
-- already 'admin' or 'member', both of which the new CHECK still allows.
-- Postgres validates the table on ADD CONSTRAINT; that scan cannot fail here,
-- and there is no data backfill, no rewrite and no down-migration need.
--
-- IN LOCKSTEP with internal/db/migrations_check_test.go's
-- TestClosedEnumChecksMatchConstants, whose role_mappings.role case pins this
-- value list against the oidc role constants themselves -- so the DDL and the
-- Go side cannot drift apart in either direction (the same guard 0051's own
-- header points at).
ALTER TABLE role_mappings DROP CONSTRAINT IF EXISTS role_mappings_role_check;

ALTER TABLE role_mappings
    ADD CONSTRAINT role_mappings_role_check
    CHECK (role IN ('admin', 'security_admin', 'member'));
