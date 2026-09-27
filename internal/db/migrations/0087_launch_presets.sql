-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Launch presets (#1143): an admin-managed, named bundle of existing
-- POST /runs fields that a launcher names instead of sending the whole spec.
-- request is the stored create-run body minus the per-launch fields (title,
-- task); the server expands it and runs the unchanged create path, so a preset
-- carries no capability of its own. version starts at 1 and moves only when a
-- write changes the row. user_types narrows who may launch it; empty is
-- every type.
CREATE TABLE IF NOT EXISTS launch_presets (
    name        TEXT PRIMARY KEY,
    version     INTEGER NOT NULL DEFAULT 1,
    description TEXT NOT NULL DEFAULT '',
    user_types  TEXT[] NOT NULL DEFAULT '{}',
    request     JSONB NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by  TEXT NOT NULL DEFAULT '',
    updated_by  TEXT NOT NULL DEFAULT ''
);

-- Which preset, at which version, a run was launched from. '' / 0 for a run
-- sent as an explicit spec and for every run created before this migration.
ALTER TABLE agent_runs
    ADD COLUMN IF NOT EXISTS preset TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS preset_version INTEGER NOT NULL DEFAULT 0;
