-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The sweep families' lease epochs. The sweeps that must run once are split into families, each led by one replica
-- at a time under a session-level advisory lock of its own (db.SweeperFamilyLockClass), which a Postgres failover can
-- release under a still-running leader. Each acquisition bumps its family's epoch, and a multi-step operation re-reads
-- it before it writes: the older leader sees a newer epoch and stops. One row per family (db.SweeperFamilies), seeded
-- here. Expand-only: sweeper_leader (0110) is kept, and every family acquisition still bumps it for older replicas.
CREATE TABLE IF NOT EXISTS sweeper_families (
    family      text PRIMARY KEY,
    epoch       bigint NOT NULL DEFAULT 0,
    holder      text NOT NULL DEFAULT '',
    acquired_at timestamptz
);

INSERT INTO sweeper_families (family) VALUES ('runs'), ('credentials'), ('audit'), ('records')
    ON CONFLICT (family) DO NOTHING;
