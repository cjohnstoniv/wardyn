-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- When each background sweep last tried and last finished without error. One row per sweep, shared by
-- every replica: the sweeps that must run once run on the elected leader, so a follower reads the
-- leader's ticks from here instead of keeping its own. Writes use GREATEST, so an older write never
-- moves a time backwards. succeeded_at is NULL until the sweep has succeeded once.
CREATE TABLE IF NOT EXISTS sweep_ticks (
    sweep        text PRIMARY KEY,
    attempted_at timestamptz NOT NULL,
    succeeded_at timestamptz,
    replica      text NOT NULL DEFAULT ''
);
