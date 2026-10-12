-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Run templates (0.9): reusable run setups, kept for one person, published to the
-- organisation, or published to one group. A template is content, never admission:
-- using one re-checks the launcher's current policy, credentials, pools and drives.
--
--   run_templates           the catalogue row: who it is for (scope plus the person
--                           or group that scope is bound to), its kind, and its
--                           current revision. Deletion is hard and cascades.
--   run_template_revisions  every saved revision, written once. The store has no
--                           path that changes a revision's content; erasure only
--                           blanks the saver's name on it.
--   template_group_admins   the bounded grant that lets one person publish to one
--                           group's templates, stored only in canonical form
--                           (TemplateGroupAdmin.Canonical). A group write re-reads
--                           it FOR SHARE inside its own transaction, so a revoke
--                           and a write never both win.
--   template_group_labels   a display label an organisation administrator gives a
--                           group. Display only: authority follows group_id.
--
-- Names are unique per audience, case-insensitively, the group's identity included,
-- so two templates offered to the same people never share a name.

CREATE TABLE IF NOT EXISTS run_templates (
  id          uuid PRIMARY KEY,
  scope       text NOT NULL CHECK (scope IN ('person', 'org', 'group')),
  owner_id    text,
  group_id    text,
  kind        text NOT NULL CHECK (kind IN ('template', 'policy')),
  name        text NOT NULL,
  description text NOT NULL DEFAULT '',
  revision    integer NOT NULL CHECK (revision >= 1),
  created_at  timestamptz NOT NULL,
  created_by  text NOT NULL,
  updated_at  timestamptz NOT NULL,
  updated_by  text NOT NULL,
  CHECK ((scope = 'person' AND owner_id IS NOT NULL AND group_id IS NULL)
      OR (scope = 'org' AND owner_id IS NULL AND group_id IS NULL)
      OR (scope = 'group' AND group_id IS NOT NULL AND owner_id IS NULL))
);

CREATE UNIQUE INDEX IF NOT EXISTS run_templates_name_per_audience
  ON run_templates (scope, COALESCE(owner_id, ''), COALESCE(group_id, ''), lower(name));
CREATE INDEX IF NOT EXISTS run_templates_owner ON run_templates (owner_id) WHERE owner_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS run_templates_group ON run_templates (group_id) WHERE group_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS run_template_revisions (
  template_id uuid NOT NULL REFERENCES run_templates (id) ON DELETE CASCADE,
  revision    integer NOT NULL,
  name        text NOT NULL,
  description text NOT NULL DEFAULT '',
  document    jsonb NOT NULL,
  created_at  timestamptz NOT NULL,
  created_by  text NOT NULL,
  PRIMARY KEY (template_id, revision)
);
CREATE INDEX IF NOT EXISTS run_template_revisions_created_by ON run_template_revisions (created_by);

CREATE TABLE IF NOT EXISTS template_group_admins (
  group_id   text NOT NULL,
  person     text NOT NULL,
  granted_by text NOT NULL,
  granted_at timestamptz NOT NULL,
  PRIMARY KEY (group_id, person)
);
CREATE INDEX IF NOT EXISTS template_group_admins_person ON template_group_admins (person);

CREATE TABLE IF NOT EXISTS template_group_labels (
  group_id text PRIMARY KEY,
  label    text NOT NULL
);
