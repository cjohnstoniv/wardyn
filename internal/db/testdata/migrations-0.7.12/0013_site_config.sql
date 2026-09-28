-- Operator-wide site config: the ONE net-new persistence surface the
-- enterprise Getting-Started enhancements introduce (upstream proxy secret
-- ref, per-ecosystem artifact-registry overrides, default SCM hosts). There is
-- exactly one site config for the operator, so the table is a classic
-- singleton: `singleton` is both the primary key and CHECKed true, which
-- makes a second row impossible at the schema level (not just app discipline).
-- The document itself is one opaque JSONB blob (types.SiteConfig), mirroring
-- the run_policies.spec / workspaces.profile precedent — no per-field columns.
CREATE TABLE IF NOT EXISTS site_config (
    singleton  BOOLEAN     NOT NULL DEFAULT true CHECK (singleton),
    config     JSONB       NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (singleton)
);
