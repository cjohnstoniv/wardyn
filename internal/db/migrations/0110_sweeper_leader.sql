-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The sweeper leader's lease epoch. The leader is elected by a session-level advisory lock
-- (db.SweeperLeaderLockKey), which a Postgres failover can release under a still-running leader, so
-- two leaders can briefly overlap. Each acquisition bumps epoch, and a multi-step operation re-reads it
-- before it writes: the older leader sees a newer epoch and stops. One row, seeded here.
CREATE TABLE IF NOT EXISTS sweeper_leader (
    id          boolean PRIMARY KEY DEFAULT true CHECK (id),
    epoch       bigint NOT NULL DEFAULT 0,
    holder      text NOT NULL DEFAULT '',
    acquired_at timestamptz
);

INSERT INTO sweeper_leader (id) VALUES (true) ON CONFLICT (id) DO NOTHING;
