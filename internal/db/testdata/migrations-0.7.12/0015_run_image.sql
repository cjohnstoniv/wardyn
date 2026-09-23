-- Persist the RESOLVED sandbox image on every run (convention image,
-- devcontainer build, workspace-built, or BYOI-wrapped) for provenance/audit.
-- Before this column, a plain run's actual image was only reconstructable from
-- deploy-time WARDYN_AGENT_IMAGES config + the agent name — not from the DB.
-- Empty for legacy rows and for runs that never reached image resolution.
ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS image TEXT NOT NULL DEFAULT '';
