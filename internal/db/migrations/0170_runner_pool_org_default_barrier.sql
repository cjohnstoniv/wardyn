-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The organisation's default barrier (0.9): the confinement class a launch that names none gets, when
-- it is at or above the caller's floor and the pool the run lands on offers it. '' means no default:
-- the strongest class offered at or above the floor, as before. It is an organisation setting on the
-- one defaults row, so it applies to every pool, Self-Hosted included.
ALTER TABLE runner_pool_org_defaults ADD COLUMN IF NOT EXISTS barrier TEXT NOT NULL DEFAULT '' CHECK (barrier IN ('', 'CC1', 'CC2', 'CC3'));
