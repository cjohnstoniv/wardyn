-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- A user type becomes a fourth subject (0.8, user-types design section 2.1):
-- capability grants, governance assignments and drive grants may be written
-- against 'user_type', with subject = the type's id (user_types.id).
--
-- Only the three subject_type CHECKs widen. No FK from subject to user_types:
-- the column is shared by every subject kind, and DeleteUserType already
-- refuses while any of these rows names the type (store_user_types.go). No
-- data changes, so ADD CONSTRAINT's validation scan cannot fail.
--
-- IN LOCKSTEP with internal/db/migrations_check_test.go's
-- TestClosedEnumChecksMatchConstants, which pins all three lists to
-- types.CapabilitySubjectType.
ALTER TABLE capability_grants DROP CONSTRAINT IF EXISTS capability_grants_subject_type_check;

ALTER TABLE capability_grants
    ADD CONSTRAINT capability_grants_subject_type_check
    CHECK (subject_type IN ('user', 'group', 'all', 'user_type'));

ALTER TABLE governance_assignments DROP CONSTRAINT IF EXISTS governance_assignments_subject_type_check;

ALTER TABLE governance_assignments
    ADD CONSTRAINT governance_assignments_subject_type_check
    CHECK (subject_type IN ('user', 'group', 'all', 'user_type'));

ALTER TABLE user_drive_grants DROP CONSTRAINT IF EXISTS user_drive_grants_subject_type_check;

ALTER TABLE user_drive_grants
    ADD CONSTRAINT user_drive_grants_subject_type_check
    CHECK (subject_type IN ('user', 'group', 'all', 'user_type'));
