-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Governance profiles (0.7 pillar): ASSIGNABLE ceilings. Wardyn has enforced
-- exactly ONE site-wide ceiling (Config.DefaultPolicy) since 0.1 -- members are
-- clamped to it, admins are not -- so a large org cannot say "the greenfield
-- group runs walled to public hosts, the platform group does not". These two
-- tables make that expressible: a named ceiling, and a row binding it to a
-- subject.
--
-- NUMBERING NOTE: the campaign plan reserved 0056 for this migration because
-- THREE unmerged branches contend for 0051-0055 (SSO's role_mappings, which is
-- now merged and holds 0051; the Fleet campaign's five, which are not in this
-- base and are themselves collapsing to one file). 0056 is unusable here:
-- internal/db's TestMigrationPrefixesNoGapsOrDupes requires CONTIGUOUS
-- prefixes, and 0052-0055 do not exist on this branch. This file therefore
-- takes the next contiguous number and the renumber-on-merge is the same
-- integration item the plan already records ("whoever merges second
-- renumbers") -- a rename is free until this has been applied to a real
-- deployment, since the filename becomes the immutable migration identity only
-- once Migrate() has recorded it as applied.
--
-- WHY ITS OWN TABLE and not a run_policies row + an assignment: run_policies
-- rows are selectable CONTENT (any signed-in caller may put one on a run), and
-- making ceilings selectable specs reproduces the exact conflation this
-- feature removes. WHY NOT a capability kind: capability_grants accumulate with
-- deny-veto semantics and capAllowed's "unenforced => allow" default, both of
-- which are meaningless (the first) or exactly backwards (the second) for a
-- ceiling.
--
-- ceiling is a types.RunPolicySpec, validatePolicySpec'd at the API write
-- boundary exactly like run_policies.spec. limits is a CLOSED Go struct
-- (types.GovernanceLimits) carrying the request-scoped autonomy switches that
-- have no home in RunPolicySpec, and it carries NO CHECK for the same reason
-- 0042 puts none on capability_grants.capability: the closed set lives in one
-- Go type validated at the write boundary, so a new switch is a struct field
-- plus its enforcement site and zero DDL, and a key this binary does not know
-- is inert (nothing asks for it).
--
-- name is UNIQUE because it is the human handle an admin assigns by and the
-- console lists by; it is also the deterministic tie-break the resolver's
-- ORDER BY ends on, so two rows sharing one would make LIMIT 1 arbitrary.
CREATE TABLE IF NOT EXISTS governance_profiles (
    id         UUID PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE,
    ceiling    JSONB NOT NULL,
    limits     JSONB NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by TEXT NOT NULL DEFAULT ''
);

-- governance_assignments binds ONE profile to ONE subject. subject_type/subject
-- name WHO with the SAME vocabulary capability_grants uses (0042) -- deliberately
-- the same closed enum, types.CapabilitySubjectType, never a second one:
--   user  -- a lowercased OIDC "sub" OR an email (capabilitySubjects returns
--           both, sub first).
--   group -- one entry of the login-time roles+groups claim snapshot.
--   all   -- every signed-in human; subject is '' for this type.
--
-- ON DELETE RESTRICT, not CASCADE: deleting a profile that still binds someone
-- would SILENTLY WIDEN every one of its members back to the deployment ceiling.
-- The FK makes that a caller-visible 409 ("unassign it first") instead of an
-- unnoticed loss of containment. This is the one place the schema, rather than
-- application code, is the enforcement point -- exactly where a DB constraint
-- belongs.
--
-- priority breaks ties WITHIN a tier (higher wins), and profiles.name breaks
-- ties after that, so the resolver's single indexed read has a TOTAL order and
-- LIMIT 1 is deterministic. The tier order itself (user > group > all, and
-- within user a sub-keyed match over an email-keyed one) is not stored -- it is
-- computed in the resolver's ORDER BY, because it depends on the CALLER's
-- subject list, not on the row.
--
-- UNIQUE(subject_type, subject) is the natural key and exactly what the CRUD
-- upsert needs: re-assigning a subject REPOINTS its one row instead of
-- accumulating a second, which would make "which profile does Bob get" depend
-- on priority/name tie-breaks rather than on the admin's last write. It also
-- serves as the resolver's index -- a btree on (subject_type, subject) answers
-- all three WHERE branches -- so there is deliberately no second index here
-- (0042 needed one only because its natural key is four columns wide).
CREATE TABLE IF NOT EXISTS governance_assignments (
    id           UUID PRIMARY KEY,
    subject_type TEXT NOT NULL CHECK (subject_type IN ('user', 'group', 'all')),
    subject      TEXT NOT NULL,
    profile_id   UUID NOT NULL REFERENCES governance_profiles(id) ON DELETE RESTRICT,
    priority     INT NOT NULL DEFAULT 0,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by   TEXT NOT NULL DEFAULT '',
    UNIQUE (subject_type, subject)
);

-- The ONE existing-table change this migration makes, and it exists because the
-- group tier can EVAPORATE silently. sessionGroups sorts and truncates a
-- session's group snapshot at 2048 bytes; a member in enough groups therefore
-- loses the group whose assignment walls them, resolves to the deployment
-- ceiling, and nothing anywhere says so. The cookie lane carries a `truncated`
-- bit for this. API TOKENS need the same bit: 0045 stamps `groups` verbatim at
-- mint and replays it into the very same capabilitySubjects the resolver reads,
-- so without this column the token lane -- the lane the API/CLI-first slice
-- ships on -- re-imports exactly the evaporation the cookie fix closed.
--
-- NULLABLE on purpose, and NULL is NOT "false". A pre-0.7 token was minted
-- before anything recorded the bit, so its snapshot's completeness is UNKNOWN;
-- it is treated as TRUNCATED wherever a group-tier assignment exists (fail
-- closed), costing legacy-token holders one re-mint on deployments that
-- actually adopt group profiles, and nothing at all on deployments that do not.
-- A DEFAULT FALSE would have silently asserted "complete" for every one of
-- them -- fail OPEN, the exact bug the column exists to prevent.
ALTER TABLE api_tokens ADD COLUMN IF NOT EXISTS groups_truncated BOOLEAN;
