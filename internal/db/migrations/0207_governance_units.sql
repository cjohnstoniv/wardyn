-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0
--
-- Governance profiles become units and governance assignments become memberships (the clean
-- break of the policy tree, 0.10.0). Pure renames: every row, grant and foreign key stays where
-- it is, so no run's walls change. A table rename keeps its grants, which is why the Split-role
-- grant lines need no change.
--
--   governance_profiles.base_profile_id      -> governance_units.parent_id
--   governance_assignments.profile_id        -> governance_memberships.unit_id
--   agent_runs.governance_profile_id         -> agent_runs.unit_id
--   run_queue.launch ->> 'profile_id'        -> 'unit_id'
--   governance_changes.target_kind           governance_profile -> governance_unit,
--                                            governance_assignment -> governance_membership
--
-- governance_changes payload and diff are rewritten key by key, so a PENDING change proposed
-- before the upgrade still reads. Its base_hash covers the renamed JSON, so it is refused as
-- stale on approval and is proposed again. audit_events rows are immutable and keep the old
-- spellings; the readers that parse them accept both.

-- Re-runnable: the renames run only while the old table exists, so a migration re-applied over a
-- migrated schema (the downgrade-refusal test makes the newest one pending again) changes nothing.
DO $mig$
BEGIN
    IF to_regclass('governance_profiles') IS NOT NULL THEN
        ALTER TABLE governance_profiles RENAME TO governance_units;
        ALTER TABLE governance_units RENAME COLUMN base_profile_id TO parent_id;
        ALTER TABLE governance_assignments RENAME TO governance_memberships;
        ALTER TABLE governance_memberships RENAME COLUMN profile_id TO unit_id;
        ALTER TABLE agent_runs RENAME COLUMN governance_profile_id TO unit_id;

        ALTER TABLE governance_units RENAME CONSTRAINT governance_profiles_pkey TO governance_units_pkey;
        ALTER TABLE governance_units RENAME CONSTRAINT governance_profiles_name_key TO governance_units_name_key;
        ALTER TABLE governance_units RENAME CONSTRAINT governance_profiles_contact_check TO governance_units_contact_check;
        ALTER TABLE governance_units RENAME CONSTRAINT governance_profiles_base_profile_id_fkey TO governance_units_parent_id_fkey;
        ALTER TABLE governance_units RENAME CONSTRAINT governance_profiles_base_needs_overlay TO governance_units_base_needs_overlay;
        ALTER TABLE governance_units RENAME CONSTRAINT governance_profiles_no_self_base TO governance_units_no_self_base;
        ALTER TABLE governance_units RENAME CONSTRAINT governance_profiles_limits_need_overlay TO governance_units_limits_need_overlay;
        ALTER TABLE governance_units RENAME CONSTRAINT governance_profiles_overlay_objects TO governance_units_overlay_objects;
        ALTER TABLE governance_units RENAME CONSTRAINT governance_profiles_composed_has_no_raw_ceiling TO governance_units_composed_has_no_raw_ceiling;

        ALTER TABLE governance_memberships RENAME CONSTRAINT governance_assignments_pkey TO governance_memberships_pkey;
        ALTER TABLE governance_memberships RENAME CONSTRAINT governance_assignments_profile_id_fkey TO governance_memberships_unit_id_fkey;
        ALTER TABLE governance_memberships RENAME CONSTRAINT governance_assignments_subject_type_check TO governance_memberships_subject_type_check;
        ALTER TABLE governance_memberships RENAME CONSTRAINT governance_assignments_subject_type_subject_key TO governance_memberships_subject_type_subject_key;
    END IF;
END
$mig$;

UPDATE run_queue
SET launch = (launch - 'profile_id') || jsonb_build_object('unit_id', launch -> 'profile_id')
WHERE launch ? 'profile_id';

UPDATE governance_changes
SET target_kind = CASE target_kind
        WHEN 'governance_profile' THEN 'governance_unit'
        WHEN 'governance_assignment' THEN 'governance_membership'
    END
WHERE target_kind IN ('governance_profile', 'governance_assignment');

-- Keys only: a JSON key is always followed by ": " in jsonb text, and a value that spells the same
-- characters carries a backslash before its quotes, so it never matches.
UPDATE governance_changes
SET payload = replace(replace(replace(replace(payload::text,
        '"base_profile_id": ', '"parent_id": '),
        '"profile_id": ', '"unit_id": '),
        '"profile_chain": ', '"unit_chain": '),
        '"profile": ', '"unit": ')::jsonb,
    diff = replace(replace(replace(replace(replace(diff::text,
        '"base_profile_id": ', '"parent_id": '),
        '"profile_id": ', '"unit_id": '),
        '"profile_chain": ', '"unit_chain": '),
        '"profile": ', '"unit": '),
        '"assignment": ', '"membership": ')::jsonb
WHERE target_kind IN ('governance_unit', 'governance_membership');
