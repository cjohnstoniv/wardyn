-- Onboarded Workspaces (plan core B1): a pre-registered, admin-reviewed local
-- dir or repo a run may attach, replacing free-text host paths/repo slugs.
-- Import scans/reviews the source ONCE (core A, a later wave) and persists a
-- profile; runs thereafter reference the workspace instead of an arbitrary
-- path. Mirrors run_policies (0001_init.sql:25-31).
CREATE TABLE IF NOT EXISTS workspaces (
    id                 UUID PRIMARY KEY,
    name               TEXT UNIQUE NOT NULL,
    kind               TEXT NOT NULL CHECK (kind IN ('local_dir','repo')),
    source             TEXT NOT NULL, -- host path (local_dir) or repo slug/URL (repo)
    ref                TEXT NOT NULL DEFAULT '', -- optional git ref (repo only)
    default_target     TEXT NOT NULL DEFAULT '', -- in-container path a run uses absent an override
    profile            JSONB, -- core A's WorkspaceProfile, opaque to core B; NULL until scanned
    image_ref          TEXT NOT NULL DEFAULT '', -- resolved/generated image for this workspace's profile
    built_profile_hash TEXT NOT NULL DEFAULT '', -- profile hash image_ref was built from (rebuild cache key)
    status             TEXT NOT NULL DEFAULT 'pending_scan'
                         CHECK (status IN ('pending_scan','ready','error')),
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- A local_dir source is unambiguous: the same host path must not be onboarded
-- twice (source -> workspace should be a function). Repos may legitimately
-- share a URL across differently-named/ref'd/targeted workspaces, so the
-- uniqueness is scoped to kind='local_dir' only.
CREATE UNIQUE INDEX IF NOT EXISTS workspaces_local_dir_source_idx
    ON workspaces (source) WHERE kind = 'local_dir';
