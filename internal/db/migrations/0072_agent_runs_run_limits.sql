-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Run limits (long-holds design rev 4, section 2.2): a run captures at create
-- the owner's profile run limits, the profile they came from, its end and its
-- wait for a decision. Every later change to the end or the wait clamps against
-- these captured bounds, and a tightened profile finds its live runs by
-- governance_profile_id.
--
-- ends_at NULL is "no end", which is every legacy row. wait_budget_sec 0 is
-- "the deployment's approval expiry", which is also every legacy row.
-- run_limits is the closed Go struct types.RunLimits, validated at the write
-- boundary, so no CHECK backs it (the 0042 doctrine). governance_profile_id
-- has no foreign key: deleting a profile must not touch the runs it governed.
-- All four adds are metadata-only on PG11+.
ALTER TABLE agent_runs
    ADD COLUMN IF NOT EXISTS ends_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS wait_budget_sec INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS run_limits JSONB NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN IF NOT EXISTS governance_profile_id UUID;
