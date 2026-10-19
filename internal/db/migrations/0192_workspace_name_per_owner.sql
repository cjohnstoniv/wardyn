-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- A workspace name is unique per owner, not across the deployment: with 0008's global
-- UNIQUE(name), a member naming a workspace like another member's private one was refused,
-- which told them that name exists. owned_by is '' on every operator-owned row (0048), so
-- operator workspaces stay unique among themselves. Every existing row already satisfies
-- the narrower index.
ALTER TABLE workspaces DROP CONSTRAINT IF EXISTS workspaces_name_key;
CREATE UNIQUE INDEX IF NOT EXISTS workspaces_owner_name_uniq ON workspaces (owned_by, name);
