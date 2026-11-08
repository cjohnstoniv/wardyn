-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Unit deciders: who besides the security tier may decide each kind of hold for runs launched in a
-- unit (PUT /governance/units/{id}/deciders). One row per unit, holding its rules as one JSON list, so
-- the unit row, its held-change hash and the stored walls do not change. A unit with no row has no
-- rule of its own and the nearest ancestor with one governs; with none anywhere, holds are decided as
-- they were without this table. A row goes with its unit.
--
-- A split-role install grants the app role SELECT, INSERT, UPDATE, DELETE on governance_unit_deciders
-- (docs/OPERATIONS.md).
--
-- Re-runnable: every statement is IF NOT EXISTS.
CREATE TABLE IF NOT EXISTS governance_unit_deciders (
    unit_id    UUID PRIMARY KEY REFERENCES governance_units (id) ON DELETE CASCADE,
    rules      JSONB NOT NULL,
    updated_by TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
