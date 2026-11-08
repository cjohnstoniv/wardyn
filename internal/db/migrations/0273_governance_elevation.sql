-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- An elevation: a membership with an end. Readers ignore a row past expires_at, so a lagging sweep
-- never extends access; the sweep deletes it. NULL for every existing row: permanent, as before.
ALTER TABLE governance_memberships ADD COLUMN IF NOT EXISTS expires_at TIMESTAMPTZ NULL;
CREATE INDEX IF NOT EXISTS governance_memberships_expires_idx
    ON governance_memberships (expires_at) WHERE expires_at IS NOT NULL;

-- A run launched under an elevation is capped at its end: a third lifetime-cap source.
ALTER TABLE agent_runs DROP CONSTRAINT IF EXISTS agent_runs_lifetime_cap_source_check;
ALTER TABLE agent_runs ADD CONSTRAINT agent_runs_lifetime_cap_source_check
    CHECK (lifetime_cap_source IN ('', 'pool', 'deployment', 'elevation'));

-- The renew_after read: the last applied elevation of one person in one unit.
CREATE INDEX IF NOT EXISTS governance_changes_target_decided
    ON governance_changes (target_kind, target_key, decided_at DESC) WHERE state = 'applied';
