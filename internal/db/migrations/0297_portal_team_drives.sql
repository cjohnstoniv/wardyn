-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Team drives for an issuer portal. Additive only: nothing here changes what an existing allocation or run means.
--
--   * user_drive_grants accepts a 'portal' subject type, whose subject is the portal's delegates.id in
--     canonical lowercase form and which never carries a home_override. Only a drive allocation names a
--     portal, so the other two subject-type CHECKs (capability_grants, governance_assignments) stay as they are.
--   * agent_runs.portal_drives marks a run that mounted a portal's drive. Such a run is reachable only through
--     the portal that created it.
--   * agent_runs_portal_drive_fence refuses a marked run whose portal row the insert did not find live. The
--     insert reads the portal row FOR SHARE into created_via, so a revoke and a create cannot both succeed
--     past each other. NOT VALID checks every new or changed row without scanning the table: every existing
--     row has the default.
--
-- No trigger and no table are added.
ALTER TABLE user_drive_grants DROP CONSTRAINT IF EXISTS user_drive_grants_subject_type_check;

ALTER TABLE user_drive_grants
    ADD CONSTRAINT user_drive_grants_subject_type_check
    CHECK (subject_type IN ('user', 'group', 'all', 'user_type', 'portal'));

ALTER TABLE user_drive_grants DROP CONSTRAINT IF EXISTS user_drive_grants_portal_subject;

ALTER TABLE user_drive_grants
    ADD CONSTRAINT user_drive_grants_portal_subject
    CHECK (subject_type <> 'portal' OR (
        subject ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' AND home_override = ''));

ALTER TABLE agent_runs
    ADD COLUMN IF NOT EXISTS portal_drives BOOLEAN NOT NULL DEFAULT false;

ALTER TABLE agent_runs DROP CONSTRAINT IF EXISTS agent_runs_portal_drive_fence;

ALTER TABLE agent_runs
    ADD CONSTRAINT agent_runs_portal_drive_fence CHECK (NOT portal_drives OR created_via IS NOT NULL) NOT VALID;
