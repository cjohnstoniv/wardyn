-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- org_walls is the organisation walls document (types.OrgWalls): one site-level row, the outermost
-- limit on every run's effective policy. enabled starts false and no row means no walls, so a
-- deployment that never writes it resolves every run as before. spec and limits hold only the knobs
-- the walls name; a knob they leave out is no wall.
CREATE TABLE IF NOT EXISTS org_walls (
    singleton  BOOLEAN PRIMARY KEY DEFAULT true CHECK (singleton),
    enabled    BOOLEAN NOT NULL DEFAULT false,
    spec       JSONB NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(spec) = 'object'),
    limits     JSONB NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(limits) = 'object'),
    updated_by TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- break_glass marks a run the admin token launched past the walls while they were on.
ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS break_glass BOOLEAN NOT NULL DEFAULT false;
