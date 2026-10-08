-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- One row per custom component: an org row (owner '') an admin wrote and grants, or a
-- person's saved row (owner = principal). Own table, not site config: person rows cannot
-- ride a whole-document PUT /site-config (carryForwardUnnamedSiteConfigFields names why), and
-- a row needs its own lifecycle, audit and erasure. definition holds secret NAMES and hosts,
-- never values. Personal data: owner rows are erased by the `components` erasure scope.
-- The id is the writer's, never a default: an org row is usable by nobody until granted, and
-- that restriction is written for the id before the row exists.
-- Split-role installs grant the app role SELECT, INSERT, UPDATE, DELETE on components.
CREATE TABLE IF NOT EXISTS components (
    id         UUID PRIMARY KEY,
    owner      TEXT NOT NULL,
    name       TEXT NOT NULL,
    definition JSONB NOT NULL,
    version    INTEGER NOT NULL CHECK (version > 0),
    created_by TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    UNIQUE (owner, name)
);
CREATE INDEX IF NOT EXISTS components_owner_idx ON components (owner);
