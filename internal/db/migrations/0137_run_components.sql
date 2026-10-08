-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The components a run was launched with, written once at create, as a snapshot: a copy of each
-- definition, not a reference, so reviving or explaining a run reads what it launched with whatever
-- has since happened to the stored component. A child table, not an agent_runs column: scanRun binds
-- columns by position and a run without components must read exactly as it did.
--
-- Each row is also the run's AUTHORIZATION TOMBSTONE, which outlives a person's erasure: revive
-- re-checks the doors a component needed (the org component's own grant, or the custom-component
-- feature for a self-defined one), and a run that lost its rows would read as having needed none.
-- Erasure therefore clears the descriptive content of a person's rows (owner, name, version,
-- definition and the id of their saved component) and keeps run_id, ordinal and self_defined. An org
-- row (owner '') is no person's and is never cleared; its component_id names the door it needs.
-- component_id is NULL for an inline (run-only) component and deliberately carries no foreign key.
-- Split-role installs grant the app role SELECT, INSERT, UPDATE on run_components.
CREATE TABLE IF NOT EXISTS run_components (
    run_id       UUID NOT NULL REFERENCES agent_runs(id) ON DELETE CASCADE,
    ordinal      INTEGER NOT NULL CHECK (ordinal >= 0),
    self_defined BOOLEAN NOT NULL,
    component_id UUID,
    owner        TEXT,
    name         TEXT,
    version      INTEGER CHECK (version >= 0),
    definition   JSONB,
    PRIMARY KEY (run_id, ordinal),
    -- Content is all there or all cleared, and only a person's row is ever cleared.
    CHECK (CASE WHEN owner IS NULL
        THEN name IS NULL AND version IS NULL AND definition IS NULL AND component_id IS NULL AND self_defined
        ELSE name IS NOT NULL AND version IS NOT NULL AND definition IS NOT NULL END),
    -- An org row is never self-defined, and a person's row always is: only the launcher's own attach.
    CHECK (owner IS NULL OR (owner = '') = NOT self_defined)
);
CREATE INDEX IF NOT EXISTS run_components_owner_idx ON run_components (owner) WHERE owner <> '';
