-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Wall presets: named, narrow-only walls a person picks at launch, kept for one person, a group or the
-- organisation. A row's spec is a personal walls document (types.PersonalWallsSpec), validated at the
-- API. Who may read and write a row is the run templates' authority (types.TemplateAuthorize) on the
-- same owner shape, so the CHECK below mirrors run_templates'. The built-in presets are constants in
-- code, never rows. No revisions table: a run keeps its own frozen copy in agent_runs.wall_preset,
-- and the audit keeps a shared preset's history.
--
-- owner_id is the canonical user subject, so the person-erasure step matches it as given.
CREATE TABLE IF NOT EXISTS wall_presets (
  id uuid PRIMARY KEY,
  scope text NOT NULL CHECK (scope IN ('person','org','group')),
  owner_id text, group_id text,
  name text NOT NULL, description text NOT NULL DEFAULT '',
  spec jsonb NOT NULL,
  version integer NOT NULL CHECK (version >= 1),
  created_at timestamptz NOT NULL, created_by text NOT NULL,
  updated_at timestamptz NOT NULL, updated_by text NOT NULL,
  CHECK ((scope='person' AND owner_id IS NOT NULL AND group_id IS NULL)
      OR (scope='org' AND owner_id IS NULL AND group_id IS NULL)
      OR (scope='group' AND group_id IS NOT NULL AND owner_id IS NULL)));
CREATE UNIQUE INDEX IF NOT EXISTS wall_presets_name_per_audience
  ON wall_presets (scope, COALESCE(owner_id,''), COALESCE(group_id,''), lower(name));
CREATE INDEX IF NOT EXISTS wall_presets_owner ON wall_presets (owner_id) WHERE owner_id IS NOT NULL;

-- The frozen copy of the preset a run was launched with, re-asserted without a store read. An older
-- server ignores the column, so it stops re-asserting a preset on running runs.
ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS wall_preset jsonb;
