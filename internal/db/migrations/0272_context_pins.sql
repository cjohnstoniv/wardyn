-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Context pins: an organisation component pinned to a unit (components.owning_unit_id), so only a run
-- whose context is that unit or a unit below it may attach it, and a strict workspace pin
-- (workspaces.owning_unit_strict), which drops the parent-unit mapping. A person's own row cannot be
-- pinned. Both default to no pin, so an upgrade changes nothing.
--
-- No grant change: the app role already has UPDATE on both tables (0136, 0227).
--
-- Re-runnable: every column is IF NOT EXISTS, and every constraint is dropped by name before it is
-- added.

ALTER TABLE components ADD COLUMN IF NOT EXISTS owning_unit_id UUID NULL;
ALTER TABLE components DROP CONSTRAINT IF EXISTS components_owning_unit_id_fkey;
ALTER TABLE components ADD CONSTRAINT components_owning_unit_id_fkey
    FOREIGN KEY (owning_unit_id) REFERENCES governance_units (id) ON DELETE RESTRICT;
ALTER TABLE components DROP CONSTRAINT IF EXISTS components_owning_unit_org_only;
ALTER TABLE components ADD CONSTRAINT components_owning_unit_org_only
    CHECK (owning_unit_id IS NULL OR owner = '');
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS owning_unit_strict BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE workspaces DROP CONSTRAINT IF EXISTS workspaces_owning_unit_strict_needs_unit;
ALTER TABLE workspaces ADD CONSTRAINT workspaces_owning_unit_strict_needs_unit
    CHECK (NOT owning_unit_strict OR owning_unit_id IS NOT NULL);
