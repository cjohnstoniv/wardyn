-- Three-tier split (owner-approved): Sources become a SHARED library (a repo/
-- dir configured once — its own requirements contract, its own scan — attached
-- to many workspaces), base images become a shared catalog (registry/custom/
-- byo; "recommended" is per-workspace DERIVED and excluded by CHECK, so the
-- rule is structural, not a convention), and the workspace becomes the
-- aggregate: ordered attachments (source refs + inline ephemerals, each with
-- target/writable/overrides) + a base-image ref + its own requirement rows.
--
-- EXPAND-ONLY. Migrations are forward-only (internal/db/db.go), so a purely
-- additive 0031 is the entire rollback story: a bad deploy reverts the binary
-- and every pre-split column is still there, untouched. The contract half
-- (dropping workspaces.sources/base_image and the scan-owned columns) is 0032,
-- which must ship in a LATER release, never this one.
--
-- Requirement rows deliberately DO NOT move: every existing row stays in the
-- workspace's own requirements column (the "overlay"), and the contract fold
-- of {no source contracts} ∪ overlay is the overlay verbatim — which makes
-- this migration provably behavior-identical for every pre-split workspace.
-- Each source's own contract fills in on its first per-source scan.

-- 1) The tier-1 library. UNIQUE (kind, locator, ref) IS the dedupe rule —
-- upserts are one ON CONFLICT statement with no read-then-write race.
-- active_run_id is the per-source scan fence, the exact job
-- workspaces.active_run_id did for whole-workspace scans before the retarget.
CREATE TABLE IF NOT EXISTS sources (
    id            UUID PRIMARY KEY,
    kind          TEXT NOT NULL CHECK (kind IN ('local_dir','repo')),
    locator       TEXT NOT NULL,
    ref           TEXT NOT NULL DEFAULT '',
    name          TEXT NOT NULL,
    requirements  JSONB,
    profile       JSONB,
    status        TEXT NOT NULL DEFAULT 'pending_scan'
                  CHECK (status IN ('pending_scan','scanning','scanned','error')),
    active_run_id UUID,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (kind, locator, ref)
);

-- 2) The tier-2 catalog. No 'recommended' — that build is derived from ONE
-- workspace's merged source profiles and has no catalog identity. The unique
-- index is the identity rule (steps normalized so NULL and [] are one thing).
CREATE TABLE IF NOT EXISTS base_images (
    id         UUID PRIMARY KEY,
    kind       TEXT NOT NULL CHECK (kind IN ('registry','custom','byo')),
    name       TEXT NOT NULL,
    image      TEXT NOT NULL,
    steps      JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS base_images_identity_idx
    ON base_images (kind, image, (COALESCE(steps, '[]'::jsonb)));

-- 3) Tier-3 columns, nullable first (backfilled below). base_image_id NULL
-- means "recommended"/derived — deliberately NOT a row. agent_runs.source_id
-- is the per-source scan run's trusted linkage, mirroring 0009's workspace_id
-- (and like it, no FK: runs outlive/predate rows freely).
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS attachments JSONB;
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS base_image_id UUID REFERENCES base_images(id);
ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS source_id UUID;

-- 4) Extract the library from every workspace's embedded sources[], deduped
-- by the identity triple. Canonicalization: dirs lose trailing slashes (but
-- "/" itself survives), repo locators lowercase, refs trim. name = the last
-- path/slug segment. jsonb_build_* does all escaping (0029's idiom).
INSERT INTO sources (id, kind, locator, ref, name, status, created_at, updated_at)
SELECT gen_random_uuid(),
       e->>'type',
       ident.locator,
       ident.ref,
       COALESCE(NULLIF(regexp_replace(ident.locator, '.*/', ''), ''), ident.locator),
       'pending_scan', now(), now()
FROM workspaces w,
     jsonb_array_elements(w.sources) AS e,
     LATERAL (SELECT
        CASE WHEN e->>'type' = 'local_dir'
             THEN CASE WHEN e->>'path' = '/' THEN '/' ELSE rtrim(e->>'path', '/') END
             ELSE lower(e->>'source') END AS locator,
        CASE WHEN e->>'type' = 'repo' THEN btrim(COALESCE(e->>'ref','')) ELSE '' END AS ref
     ) AS ident
WHERE e->>'type' IN ('local_dir','repo')
ON CONFLICT (kind, locator, ref) DO NOTHING;

-- 5) Build each workspace's attachments, preserving order (WITH ORDINALITY),
-- per-attachment target/writable carried over, ephemeral rows inline. A
-- local_dir's writable flag was per-source before the split and is
-- per-attachment after it — same value, new owner.
UPDATE workspaces w SET attachments = att.rows
FROM (
    SELECT w2.id AS wid,
           jsonb_agg(
             jsonb_strip_nulls(
               CASE WHEN e->>'type' = 'ephemeral' THEN
                 jsonb_build_object(
                   'ephemeral', true,
                   'target', NULLIF(e->>'target',''))
               ELSE
                 jsonb_build_object(
                   'source_id', s.id,
                   'target', NULLIF(e->>'target',''),
                   'writable', CASE WHEN (e->>'writable')::boolean THEN true ELSE NULL END)
               END
             ) ORDER BY ord) AS rows
    FROM workspaces w2,
         jsonb_array_elements(w2.sources) WITH ORDINALITY AS a(e, ord)
         LEFT JOIN sources s
           ON e->>'type' IN ('local_dir','repo')
          AND s.kind = e->>'type'
          AND s.locator = CASE WHEN e->>'type' = 'local_dir'
                               THEN CASE WHEN e->>'path' = '/' THEN '/' ELSE rtrim(e->>'path','/') END
                               ELSE lower(e->>'source') END
          AND s.ref = CASE WHEN e->>'type' = 'repo' THEN btrim(COALESCE(e->>'ref','')) ELSE '' END
    GROUP BY w2.id
) att
WHERE att.wid = w.id;

-- A workspace with no sources rows at all (unreachable — sources is NOT NULL
-- and never empty — but cheap to make impossible): empty attachments array.
UPDATE workspaces SET attachments = '[]'::jsonb WHERE attachments IS NULL;
ALTER TABLE workspaces ALTER COLUMN attachments SET NOT NULL;

-- 6) Extract the catalog from embedded base_image for the three reusable
-- kinds, deduped on the identity index; point base_image_id at the row.
-- 'recommended' and NULL leave base_image_id NULL — that IS the marker.
INSERT INTO base_images (id, kind, name, image, steps, created_at, updated_at)
SELECT gen_random_uuid(),
       w.base_image->>'kind',
       COALESCE(NULLIF(regexp_replace(w.base_image->>'image', '.*/', ''), ''), w.base_image->>'image'),
       w.base_image->>'image',
       w.base_image->'steps',
       now(), now()
FROM workspaces w
WHERE w.base_image->>'kind' IN ('registry','custom','byo')
ON CONFLICT (kind, image, (COALESCE(steps, '[]'::jsonb))) DO NOTHING;

UPDATE workspaces w SET base_image_id = b.id
FROM base_images b
WHERE w.base_image->>'kind' IN ('registry','custom','byo')
  AND b.kind  = w.base_image->>'kind'
  AND b.image = w.base_image->>'image'
  AND COALESCE(b.steps, '[]'::jsonb) = COALESCE(w.base_image->'steps', '[]'::jsonb);

-- 7) Carry scan results onto the source ONLY where attribution is
-- unambiguous: the workspace has exactly one non-ephemeral attachment and the
-- source hasn't been scanned yet. A multi-source workspace's MERGED profile
-- cannot be split back onto its members — those sources stay pending_scan and
-- one rescan click rebuilds honestly (population today: ~zero; the
-- composition model is days old).
UPDATE sources s
SET profile = w.profile,
    status  = w.status,
    updated_at = now()
FROM (
    SELECT (a.e->>'source_id')::uuid AS sid, w2.profile, w2.status
    FROM workspaces w2,
         jsonb_array_elements(w2.attachments) AS a(e)
    WHERE w2.profile IS NOT NULL
      AND w2.status IN ('scanned','error')
      AND a.e ? 'source_id'
      AND (SELECT count(*) FROM jsonb_array_elements(w2.attachments) x
            WHERE x ? 'source_id') = 1
) w
WHERE s.id = w.sid AND s.profile IS NULL;

-- 8) The legacy embedded column may go NULL for rows written after the split
-- (the store keeps writing it until the API flips to attachments, but new
-- code paths must be free to stop). Dropped entirely in 0032.
ALTER TABLE workspaces ALTER COLUMN sources DROP NOT NULL;

-- 9) The in-use containment index: DELETE /sources/{id} answers "who attaches
-- this" with one GIN probe, and refuses loudly rather than orphaning the
-- mount gate.
CREATE INDEX IF NOT EXISTS workspaces_attachments_gin
    ON workspaces USING gin (attachments jsonb_path_ops);
