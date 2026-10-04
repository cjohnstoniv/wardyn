-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- A governance profile may be COMPOSED: a base (another profile, or the
-- deployment default when base_profile_id is NULL) plus an overlay that can only
-- narrow it. overlay IS NULL is a standalone profile, read exactly as before.
-- A composed row stores no raw ceiling or limits, so nothing can mistake its
-- columns for authority: the resolver (internal/api/governance_compose.go) is
-- the only reader of a composed profile's authority.
--
-- ON DELETE RESTRICT makes deleting a base that still has children a 409, as the
-- assignment FK already does for an assigned profile. All columns are nullable
-- with no default and no backfill, so every existing row satisfies every CHECK. Each CHECK is
-- dropped before it is added so the file can run twice (break-glass migrate).
ALTER TABLE governance_profiles
    ADD COLUMN IF NOT EXISTS base_profile_id UUID NULL REFERENCES governance_profiles(id) ON DELETE RESTRICT,
    ADD COLUMN IF NOT EXISTS overlay         JSONB NULL,
    ADD COLUMN IF NOT EXISTS overlay_limits  JSONB NULL,
    DROP CONSTRAINT IF EXISTS governance_profiles_base_needs_overlay,
    ADD CONSTRAINT governance_profiles_base_needs_overlay
        CHECK (base_profile_id IS NULL OR overlay IS NOT NULL),
    DROP CONSTRAINT IF EXISTS governance_profiles_no_self_base,
    ADD CONSTRAINT governance_profiles_no_self_base
        CHECK (base_profile_id IS DISTINCT FROM id),
    DROP CONSTRAINT IF EXISTS governance_profiles_limits_need_overlay,
    ADD CONSTRAINT governance_profiles_limits_need_overlay
        CHECK (overlay_limits IS NULL OR overlay IS NOT NULL),
    DROP CONSTRAINT IF EXISTS governance_profiles_overlay_objects,
    ADD CONSTRAINT governance_profiles_overlay_objects
        CHECK ((overlay IS NULL OR jsonb_typeof(overlay) = 'object')
           AND (overlay_limits IS NULL OR jsonb_typeof(overlay_limits) = 'object')),
    DROP CONSTRAINT IF EXISTS governance_profiles_composed_has_no_raw_ceiling,
    ADD CONSTRAINT governance_profiles_composed_has_no_raw_ceiling
        CHECK (overlay IS NULL OR (ceiling = '{}'::jsonb AND limits = '{}'::jsonb));
