-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Unit admins and locks (the policy tree's scoped admins). A governance_unit_admins row makes a person
-- or an IdP group the admin of one unit and the units below it. locked_knobs on a unit or on the
-- boundary names the knobs no write below it may change, a narrowing included.
--
-- A split-role install grants the app role SELECT, INSERT, UPDATE, DELETE on governance_unit_admins
-- (docs/OPERATIONS.md).
--
-- Re-runnable: every statement is IF NOT EXISTS.
CREATE TABLE IF NOT EXISTS governance_unit_admins (
    id           UUID PRIMARY KEY,
    unit_id      UUID NOT NULL REFERENCES governance_units (id) ON DELETE CASCADE,
    subject_type TEXT NOT NULL CHECK (subject_type IN ('user', 'group')),
    subject      TEXT NOT NULL,
    granted_by   TEXT NOT NULL,
    approved_by  TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (unit_id, subject_type, subject)
);

CREATE INDEX IF NOT EXISTS governance_unit_admins_subject
    ON governance_unit_admins (subject_type, subject);

-- A constant default, so neither ALTER rewrites a row.
ALTER TABLE governance_units ADD COLUMN IF NOT EXISTS locked_knobs TEXT[] NOT NULL DEFAULT '{}';
ALTER TABLE governance_boundary ADD COLUMN IF NOT EXISTS locked_knobs TEXT[] NOT NULL DEFAULT '{}';
