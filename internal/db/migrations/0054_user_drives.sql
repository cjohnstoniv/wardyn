-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- User drives (0.7): per-user storage a member can MOUNT into a run. Nothing in
-- Wardyn allocates, names or mounts a per-user directory today -- the only
-- per-user mount facet is a narrowing allowlist over a path the MEMBER TYPES,
-- and the k8s runner refuses host binds outright -- so a corporation whose
-- people already have a home directory on a NAS has no way to put it in front
-- of an agent. These two tables make that expressible: a named, admin-registered
-- drive, and a row allocating it to a subject.
--
-- TWO KINDS, DERIVED FROM backend AND NEVER STORED BESIDE IT. A MANAGED drive
-- (docker_volume, k8s_pvc) is one Wardyn allocates for the member; a SHARE
-- (host_path, k8s_pvc_static) is a tree the platform already mounts and Wardyn
-- only binds a per-user subdirectory of. types.DriveBackend.Kind() derives it:
-- two columns that must agree are two columns that can disagree, and the
-- disagreement would decide whether the control plane CREATES an object or
-- refuses because one is missing.
--
-- WHY ITS OWN TABLE PAIR AND NOT SiteConfig. The obvious cheaper home is a
-- drives array on the site_config row, and it is wrong for three reasons the
-- tree has already paid for once:
--   1. WHOLE-DOCUMENT PUT ERASES WHAT OLDER CLIENTS DO NOT KNOW.
--      handlePutSiteConfig already carries Integrations and OnboardingCompletedAt
--      forward from the stored row precisely because a GET-then-PUT by an older
--      console or CLI blanked them -- its own comment calls that "the exact
--      footgun already solved once". A drives array would be the third instance,
--      and the blanked field would be an allocation people are mid-run on.
--   2. NO FOREIGN KEY. A grant would carry a drive key matched by STRING, which
--      loses the ON DELETE RESTRICT below -- the one property that makes
--      deleting a still-allocated drive a caller-visible 409 rather than a
--      silent widening.
--   3. AUDIT GRANULARITY. site_config.write is one row with counts; registering
--      or deleting a drive deserves its own audit row.
-- The cost, stated: drives do not ride `wardyn site-config get/apply` across a
-- reset. Neither do governance profiles or stored policies.
--
-- name is UNIQUE because it is the human handle an admin allocates by, the
-- fragment a PVC's name carries, and the deterministic tie-break the resolver's
-- ORDER BY ends on -- two rows sharing one would make LIMIT 1 arbitrary.
--
-- Three CHECK enums here, each pinned against its Go constant set by
-- internal/db's TestClosedEnumChecksMatchConstants, the same lockstep 0053's
-- role list is held in. host_root/storage_class carry NO CHECK: their shape is
-- validated by types.ValidateUserDrive at the write boundary and their SECURITY
-- ceiling is an operator-set env allowlist (WARDYN_USER_DRIVE_HOST_ROOTS), which
-- is deliberately not a database concern -- the 0042 doctrine, one closed Go
-- definition validated at the write boundary and zero DDL for the next field.
CREATE TABLE IF NOT EXISTS user_drives (
    id            UUID PRIMARY KEY,
    name          TEXT NOT NULL UNIQUE,
    backend       TEXT NOT NULL CHECK (backend IN ('docker_volume', 'host_path', 'k8s_pvc', 'k8s_pvc_static')),
    host_root     TEXT NOT NULL DEFAULT '',
    storage_class TEXT NOT NULL DEFAULT '',
    home_template TEXT NOT NULL DEFAULT 'hash' CHECK (home_template IN ('hash', 'sub', 'email_local')),
    size_mib      INT NOT NULL DEFAULT 0,
    writable      BOOLEAN NOT NULL DEFAULT false,
    reclaim       TEXT NOT NULL DEFAULT 'retain' CHECK (reclaim IN ('retain', 'delete')),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by    TEXT NOT NULL DEFAULT ''
);

-- user_drive_grants allocates ONE drive to ONE subject. subject_type/subject
-- name WHO with the SAME closed vocabulary capability_grants (0042) and
-- governance_assignments (0052) use -- types.CapabilitySubjectType, never a
-- second enum meaning the same three things:
--   user  -- a lowercased OIDC "sub" OR an email (capabilitySubjects returns
--           both, sub first).
--   group -- one entry of the login-time roles+groups claim snapshot.
--   all   -- every signed-in human; subject is '' for this type.
--
-- ON DELETE RESTRICT, not CASCADE, for the reason 0052 states and one more of
-- its own: cascading would drop the allocations of a drive an admin deleted by
-- mistake, and the DIRECTORIES those allocations name would still exist on the
-- share holding somebody's work, now unreachable and unaudited. The FK makes
-- that a 409 ("remove its allocations first"), which is the deliberate act the
-- constraint exists to force.
--
-- THE HOME NAME IS NOT A COLUMN. Where a member's bytes live is DERIVED at
-- resolve time by types.DriveHomeName -- a truncated sha256 of the drive id and
-- the subject for a managed drive, or the named claim for a share -- so there is
-- no third table to keep consistent and no row that can drift from the identity
-- it names. home_override is the ONE stored exception and it is user-tier only:
-- an admin stating "Bob's directory on the NAS is bsmith" is a fact about a
-- filesystem Wardyn does not own, and no derivation may out-vote it. On a group
-- or all row the same field would hand an entire group ONE directory, which is
-- the isolation a per-user subdirectory buys, removed by a field that reads like
-- a convenience -- types.ValidateUserDriveGrant refuses it.
--
-- writable_override is NULLABLE and NULL IS NOT false: NULL means "use the
-- drive's posture", while an explicit false is an admin saying "this subject
-- reads only" on a writable drive. A NOT NULL DEFAULT false would have made
-- every unset override silently read-only, and an override may only ever NARROW
-- the drive's own posture at run time.
--
-- UNIQUE(subject_type, subject) is the natural key and exactly what the CRUD
-- upsert needs: re-allocating a subject REPOINTS its one row instead of
-- accumulating a second. It is also THE RESOLVER'S INDEX -- a btree on
-- (subject_type, subject) answers all three WHERE branches of the single ranked
-- read -- so it doubles as both and there is no third index for either job. One
-- drive per principal in v1 falls out of it together with the resolver's LIMIT 1.
CREATE TABLE IF NOT EXISTS user_drive_grants (
    id                UUID PRIMARY KEY,
    subject_type      TEXT NOT NULL CHECK (subject_type IN ('user', 'group', 'all')),
    subject           TEXT NOT NULL,
    drive_id          UUID NOT NULL REFERENCES user_drives(id) ON DELETE RESTRICT,
    priority          INT NOT NULL DEFAULT 0,
    size_mib_override INT NOT NULL DEFAULT 0,
    writable_override BOOLEAN,
    home_override     TEXT NOT NULL DEFAULT '',
    enabled           BOOLEAN NOT NULL DEFAULT true,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by        TEXT NOT NULL DEFAULT '',
    UNIQUE (subject_type, subject)
);

-- The one index the natural key does NOT serve: "how many allocations does this
-- drive have", which the admin table renders per row and which the RESTRICT
-- above makes load-bearing -- an admin about to delete a drive is told it is
-- still allocated before Postgres tells them. Without it that count is a
-- sequential scan per drive on the console's main read.
CREATE INDEX IF NOT EXISTS user_drive_grants_drive_id_idx ON user_drive_grants (drive_id);
