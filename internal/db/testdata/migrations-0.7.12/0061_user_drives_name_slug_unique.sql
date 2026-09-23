-- 0061: one storage-object name belongs to one drive.
--
-- THE HOLE. user_drives.name is UNIQUE, and for every backend whose object name
-- Wardyn MINTS the name that actually addresses storage is not the name -- it is
-- types.DriveSlug(name), folded to the DNS-1123 fragment a PVC name may carry:
-- lowercased, every run of non-[a-z0-9] collapsed to "-", trimmed, capped at 40.
-- That fold is deliberate WITHIN a row (a cosmetic rename must not re-home
-- anybody, which is why driveIdentityFields compares through it) and it is a
-- COLLISION across two rows. UNIQUE(name) admits "Corp NAS" and "corp nas" as
-- two names; both mint wardyn-drive-corp-nas-<home>. So do "Corp NAS (eng)" and
-- "corp-nas-eng". Two drives then hand ONE storage object to two sets of
-- members, each with its own size ceiling, writable flag and reclaim policy, and
-- 0059's "one directory, one principal" fails in the namespace it claims --
-- caught today only at mount time, by the runners' wardyn.drive label check,
-- as somebody's run failing.
--
-- name_slug IS WRITTEN BY GO, NOT DERIVED HERE. store.UpsertUserDrive passes
-- types.DriveSlug(name) as a parameter on every insert and update, so the value
-- the index enforces is the value the object name is built from, by definition
-- and forever. A generated column or a functional index would have had to
-- restate that fold in SQL, where lower() is collation-dependent (under a
-- Turkish collation 'I' lowercases to a character the class does not admit) and
-- a regexp range is too -- a second definition that could disagree with the
-- first about what a drive is called. There is exactly one place that decides.
--
-- The expression below is used ONCE, to backfill rows written before this
-- migration, and never again. COLLATE "C" pins lower() and the character class
-- to ASCII, which is what the Go side does; the ORDER matches DriveSlug exactly
-- (fold, trim, cap at 40, trim again -- the second trim is why a cap that lands
-- mid-run cannot leave a trailing "-"). A backfilled value that somehow differed
-- would be corrected the next time that drive is written, because Go writes the
-- column from then on.
--
-- ONE CLASS DIFFERS AND IS NAMED RATHER THAN DISCOVERED: a character whose
-- UNICODE lowercase is ASCII (U+0130 "I with dot above" is the reachable
-- example) becomes a letter on the Go side and a "-" here, so a legacy row
-- carrying one is backfilled with a coarser slug than Go would mint. Two
-- consequences, both bounded: such a row rejoins the exact rule the next time it
-- is written, and if the coarser value collides with another row's this
-- migration FAILS the upgrade -- with the same remedy a genuine collision has,
-- below. It is not silently wrong in either direction. Pinned from the Go side
-- by TestDriveSlugIsStableForTheColumn.
ALTER TABLE user_drives ADD COLUMN IF NOT EXISTS name_slug TEXT NOT NULL DEFAULT '';

UPDATE user_drives
   SET name_slug = btrim(left(btrim(regexp_replace(lower(name COLLATE "C"), '[^a-z0-9]+', '-', 'g'), '-'), 40), '-')
 WHERE name_slug = '';

-- PARTIAL, on the namespace the collision is actually in: host_path's object
-- name is <host_root>/<home> and carries no slug at all, so two shares may hold
-- names that fold together and nothing collides. (The cross-row rule host_path
-- DOES need is the home-naming agreement store.UpsertUserDrive enforces.) The
-- other three backends all mint wardyn-drive-<slug>-<home>; they are not
-- partitioned further, because types.ValidateUserDrive requires a drive's
-- backend to match the deployment's own runner target, so the Wardyn-named
-- drives in one deployment are always on one substrate -- and the object NAME is
-- what an operator's reclaim and offboarding runbooks grep for either way.
--
-- name_slug <> '' guards the index against a row this migration could not fold
-- (it cannot exist: ValidateUserDrive refuses a name whose slug is empty), so an
-- upgrade cannot fail on a legacy row nobody can fix from the API.
--
-- AN UPGRADE CAN STILL FAIL HERE, deliberately: a deployment that already holds
-- two drives whose names fold together is already handing one object to two sets
-- of members, and this index is how that stops being invisible. The remedy is to
-- rename one of them -- psql: SELECT name_slug, array_agg(name) FROM user_drives
-- WHERE backend <> 'host_path' AND name_slug <> '' GROUP BY 1 HAVING count(*) > 1;
CREATE UNIQUE INDEX IF NOT EXISTS user_drives_name_slug_uniq
    ON user_drives (name_slug)
 WHERE backend <> 'host_path' AND name_slug <> '';
