-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- A fresh authenticated session replaces the previous incomplete inventory.
-- Candidate rows stay in PostgreSQL, not an unbounded Hub allocation.
CREATE TABLE IF NOT EXISTS runner_inventories (
    runner_id UUID PRIMARY KEY REFERENCES runners(id) ON DELETE CASCADE,
    session_id UUID NOT NULL,
    owner TEXT NOT NULL,
    inventory_id UUID,
    last_run_id UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000',
    complete BOOLEAN NOT NULL DEFAULT false,
    needs_reconciliation BOOLEAN NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE IF NOT EXISTS runner_inventory_candidates (
    runner_id UUID NOT NULL REFERENCES runner_inventories(runner_id) ON DELETE CASCADE,
    run_id UUID NOT NULL REFERENCES agent_runs(id) ON DELETE CASCADE,
    run_version TEXT NOT NULL,
    ref TEXT NOT NULL,
    generation UUID NOT NULL,
    seen BOOLEAN NOT NULL DEFAULT false,
    PRIMARY KEY (runner_id, run_id)
);

-- Each assertion and its reconciliation requirement commit with page progress.
-- These are observations, never execution or evidence-retirement authority.
CREATE TABLE IF NOT EXISTS runner_inventory_observations (
    runner_id UUID NOT NULL REFERENCES runner_inventories(runner_id) ON DELETE CASCADE,
    run_id UUID NOT NULL,
    assertion JSONB NOT NULL,
    containment JSONB,
    needs_loss BOOLEAN NOT NULL DEFAULT false,
    PRIMARY KEY (runner_id, run_id)
);
