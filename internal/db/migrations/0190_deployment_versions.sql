-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The releases this deployment has booted (0.9.3), so setup status can name the defaults an upgrade
-- changed since the release it last ran. wardynd writes one row per release at boot, after migrating
-- (db.RecordDeploymentVersion): prior_migration is the newest migration the database held before that
-- release first migrated it, '' on a fresh database. The release booted before this one is the row with
-- the latest first_booted_at before its own; for the first release that keeps this table, the prior
-- migration (and the newest run's proxy_release) stand in for it.
CREATE TABLE IF NOT EXISTS deployment_versions (
    version         TEXT        PRIMARY KEY,
    prior_migration TEXT        NOT NULL DEFAULT '',
    first_booted_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
