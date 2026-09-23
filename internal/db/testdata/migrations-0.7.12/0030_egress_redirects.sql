-- Egress Redirection generalization (owner-approved): SiteConfig.ArtifactOverrides
-- (map[string]ArtifactOverride keyed by ecosystem: npm|pip|cargo|maven|go|nuget)
-- is superseded by EgressRedirects ([]EgressRedirect{From,To,TokenSecretRef,
-- Ecosystem}), generalizing an ecosystem-keyed package-registry mirror into any
-- outbound URL/host/IP redirect (a container registry, a telemetry endpoint,
-- ...). See types.go (EgressRedirect) for the Go shape this jsonb column
-- carries opaquely -- site_config is one opaque JSONB blob (0013_site_config.sql),
-- no per-field columns, the same precedent 0029's own header cites for
-- workspaces.sources/base_image/requirements.
--
-- This migrates the ONE existing site_config row (a singleton, PK CHECKed true
-- -- see 0013): for each key of the stored artifact_overrides object, emit one
-- egress_redirects array entry:
--
--   {from: <ecosystem's public registry URL>, to: <its base_url>,
--    token_secret_ref: <its token_secret_ref, if any>, ecosystem: <the key>}
--
-- From-URL mapping (grepped from workspacescan/markers.go's egress* literals /
-- PublicRegistryHosts -- NOT invented; the literal Go table this mirrors is
-- api.ecosystemPublicURL, site_config.go, which the live PUT /site-config
-- decode-fold uses for the same purpose on a legacy REQUEST body):
--   npm   -> https://registry.npmjs.org/            (egressNPM[0])
--   pip   -> https://pypi.org/simple/                (egressPyPI[0] host, pip's
--            actual index path; files.pythonhosted.org is the file-CDN half of
--            the same public pair, never a URL pip's index-url points at)
--   cargo -> https://index.crates.io/                (egressCargo[2], the
--            sparse-index host cargo's [registries.*].index actually names)
--   maven -> https://repo.maven.apache.org/maven2/   (egressMaven[0])
--   go    -> https://proxy.golang.org                (egressGo[0])
--   nuget -> https://api.nuget.org/v3/index.json     (egressNuGet[0])
--
-- jsonb_agg(... ORDER BY key) makes the emitted array's element order match
-- exactly what the old Go code's `slices.Sorted(maps.Keys(...))` iteration
-- produced everywhere it read ArtifactOverrides (artifactMirrorRows,
-- planArtifactRedirect, artifactRepoCheck): the dedupe-by-host consumers that
-- pick a "first sighted" winner among ecosystems sharing one host depend on
-- this stored order for byte-identical post-migration dispatch behavior.
--
-- upstream_proxy_secret_ref is left untouched: it stays valid as-is. The new
-- plain upstream_proxy_url field is purely additive and needs no backfill --
-- no prior data to move, it did not exist before this release.
UPDATE site_config
SET config = (config - 'artifact_overrides') || jsonb_build_object(
    'egress_redirects',
    (SELECT jsonb_agg(
                jsonb_strip_nulls(jsonb_build_object(
                    'from', CASE key
                        WHEN 'npm'   THEN 'https://registry.npmjs.org/'
                        WHEN 'pip'   THEN 'https://pypi.org/simple/'
                        WHEN 'cargo' THEN 'https://index.crates.io/'
                        WHEN 'maven' THEN 'https://repo.maven.apache.org/maven2/'
                        WHEN 'go'    THEN 'https://proxy.golang.org'
                        WHEN 'nuget' THEN 'https://api.nuget.org/v3/index.json'
                    END,
                    'to', value ->> 'base_url',
                    'token_secret_ref', NULLIF(value ->> 'token_secret_ref', ''),
                    'ecosystem', key
                ))
                ORDER BY key
            )
     FROM jsonb_each(config -> 'artifact_overrides'))
)
WHERE singleton AND config ? 'artifact_overrides';
