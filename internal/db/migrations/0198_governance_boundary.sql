-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- governance_boundary is the Wardyn boundary (types.Boundary): one site-level row, the outermost
-- layer of every run's effective policy. No row means no_limits, so a deployment that never writes
-- it resolves every run as before. spec and limits hold only the knobs the boundary names; a knob
-- it leaves out is no limit.
CREATE TABLE IF NOT EXISTS governance_boundary (
    singleton  BOOLEAN PRIMARY KEY DEFAULT true CHECK (singleton),
    mode       TEXT NOT NULL CHECK (mode IN ('no_limits', 'hard_limits')),
    spec       JSONB NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(spec) = 'object'),
    limits     JSONB NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(limits) = 'object'),
    contact    JSONB NULL CHECK (contact IS NULL OR octet_length(contact::text) <= 8192),
    updated_by TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- break_glass marks an admin-token break-glass run (types.AgentRun.BreakGlass).
ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS break_glass BOOLEAN NOT NULL DEFAULT false;
