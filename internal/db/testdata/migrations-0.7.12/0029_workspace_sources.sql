-- Workspace Composition (owner-approved): a workspace is no longer ONE source
-- with a kind. It is a COMPOSITION of one-or-more sources (local_dir | repo |
-- ephemeral; multiples of the same type allowed; minimum one — an ephemeral
-- scratch dir is the floor), plus a base image choice, plus a requirements
-- contract. Container-kind dies: an old container workspace becomes
-- {ephemeral source} + {base_image: custom, image: <the old source>}.
--
-- sources/base_image/requirements ride the existing opaque-blob precedent
-- (profile, approved_egress, llm_cred) — no new tables. See types.go
-- (WorkspaceSource, WorkspaceBaseImage, WorkspaceRequirement) for the Go
-- shapes these columns carry opaquely. record_results and llm_cred are
-- untouched by this migration.

-- 1) Add the three new columns nullable first. sources is backfilled below
-- (step 2) then locked NOT NULL once every row has a value (step 3);
-- base_image/requirements stay nullable, like profile/approved_egress.
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS sources JSONB;
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS base_image JSONB;
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS requirements JSONB;

-- 2) Backfill sources (+ base_image for the container case) from the old
-- single-source columns, one UPDATE per legacy kind (the kind CHECK admits
-- exactly these three, so together they cover every row). jsonb_build_object/
-- array do the JSON escaping — no hand-rolled string concatenation that could
-- break on a path/slug containing a quote.
UPDATE workspaces SET sources = jsonb_build_array(jsonb_build_object(
    'type', 'local_dir',
    'path', source,
    'target', COALESCE(NULLIF(default_target, ''), '/home/agent/work'),
    'writable', writable
)) WHERE kind = 'local_dir';

-- repo's ref/target are NOT NULL DEFAULT '' columns (0008/0011); an unset one
-- becomes JSON null via NULLIF then is stripped entirely so the decoded
-- WorkspaceSource leaves Ref/Target at their Go zero value rather than storing
-- an explicit empty string.
UPDATE workspaces SET sources = jsonb_build_array(jsonb_strip_nulls(jsonb_build_object(
    'type', 'repo',
    'source', source,
    'ref', NULLIF(ref, ''),
    'target', NULLIF(default_target, '')
))) WHERE kind = 'repo';

-- container had no mount at all — Source WAS the image ref. That becomes an
-- ephemeral scratch source (the composition floor) plus a custom base image
-- pointing at the old image ref.
UPDATE workspaces SET
    sources = jsonb_build_array(jsonb_build_object('type', 'ephemeral', 'target', '/home/agent/work')),
    base_image = jsonb_build_object('kind', 'custom', 'image', source)
WHERE kind = 'container';

-- 3) Every row now has a sources[] (every pre-existing kind was handled by one
-- of the three UPDATEs above); lock it NOT NULL going forward.
ALTER TABLE workspaces ALTER COLUMN sources SET NOT NULL;

-- 4) Collapse the Workspace Import v2 pipeline statuses (0011: building,
-- verifying, verify_failed, build_error, ready) back to the pre-import-v2
-- scan-only lifecycle the composition model returns to: `scanned` when a
-- profile already exists (ready/verifying/verify_failed/building all imply a
-- successful scan happened), `pending_scan` otherwise (a build_error before
-- any scan completed). scanning and error pass through unchanged.
UPDATE workspaces SET status = CASE WHEN profile IS NOT NULL THEN 'scanned' ELSE 'pending_scan' END
    WHERE status IN ('ready', 'building', 'verifying', 'verify_failed', 'build_error');

ALTER TABLE workspaces DROP CONSTRAINT IF EXISTS workspaces_status_check;
ALTER TABLE workspaces ADD CONSTRAINT workspaces_status_check
    CHECK (status IN ('pending_scan', 'scanning', 'scanned', 'error'));

-- 5) The local_dir-source uniqueness (0008's workspaces_local_dir_source_idx)
-- was never a security property — just a tidiness convenience so the same
-- host path wasn't onboarded twice under two names. Under sources[] there is
-- no longer a single `source` column a partial index can key on (a workspace
-- may carry several local_dir sources, or none). The gate that actually
-- matters — whether a host path may be mounted into a sandbox at all — is the
-- onboarded-workspace MOUNT GATE (membership: is this path one of an
-- onboarded workspace's Sources), which lives in application code and is
-- untouched by this migration.
DROP INDEX IF EXISTS workspaces_local_dir_source_idx;

-- 6) Drop the old single-source columns and their now-orphaned kind CHECK
-- (0016's workspaces_kind_check) now that sources[]/base_image carry this
-- information. setup_commands/verify_result/verified_profile_hash/verified_at
-- were the Workspace Import v2 (0011) columns; that pipeline is retired (see
-- step 4).
ALTER TABLE workspaces DROP CONSTRAINT IF EXISTS workspaces_kind_check;
ALTER TABLE workspaces
    DROP COLUMN IF EXISTS kind,
    DROP COLUMN IF EXISTS source,
    DROP COLUMN IF EXISTS ref,
    DROP COLUMN IF EXISTS default_target,
    DROP COLUMN IF EXISTS writable,
    DROP COLUMN IF EXISTS setup_commands,
    DROP COLUMN IF EXISTS verify_result,
    DROP COLUMN IF EXISTS verified_profile_hash,
    DROP COLUMN IF EXISTS verified_at;
