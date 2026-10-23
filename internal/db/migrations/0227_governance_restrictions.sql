-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Restrictions (types.Restriction, the policy tree's deny-only lists): a restriction follows the
-- people it names into every unit they run in, and never counts as a context. spec and limits hold
-- only the knobs a restriction may carry; a knob they leave out is no limit.
--
-- A split-role install grants the app role SELECT, INSERT, UPDATE, DELETE on governance_restrictions
-- and governance_restriction_members (docs/OPERATIONS.md).
--
-- Re-runnable: every statement is IF NOT EXISTS, and the one foreign key is dropped by name before
-- it is added.
CREATE TABLE IF NOT EXISTS governance_restrictions (
    id         UUID PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE,
    spec       JSONB NOT NULL CHECK (jsonb_typeof(spec) = 'object'),
    limits     JSONB NOT NULL CHECK (jsonb_typeof(limits) = 'object'),
    contact    JSONB NULL CHECK (contact IS NULL OR octet_length(contact::text) <= 8192),
    created_by TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS governance_restriction_members (
    id             UUID PRIMARY KEY,
    restriction_id UUID NOT NULL REFERENCES governance_restrictions (id) ON DELETE CASCADE,
    subject_type   TEXT NOT NULL CHECK (subject_type IN ('user', 'group', 'user_type', 'all')),
    subject        TEXT NOT NULL,
    created_by     TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (restriction_id, subject_type, subject)
);

CREATE INDEX IF NOT EXISTS governance_restriction_members_subject
    ON governance_restriction_members (subject_type, subject);

-- owning_unit_id pins a workspace to a unit: runs on it run in that unit's walls (B1). RESTRICT, so a
-- pinned unit cannot be deleted from under its workspaces.
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS owning_unit_id UUID NULL;
ALTER TABLE workspaces DROP CONSTRAINT IF EXISTS workspaces_owning_unit_id_fkey;
ALTER TABLE workspaces ADD CONSTRAINT workspaces_owning_unit_id_fkey
    FOREIGN KEY (owning_unit_id) REFERENCES governance_units (id) ON DELETE RESTRICT;

-- restriction_ids are the restrictions a governed run captured (types.AgentRun.RestrictionIDs).
ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS restriction_ids UUID[] NOT NULL DEFAULT '{}';
