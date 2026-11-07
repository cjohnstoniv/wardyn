-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The latest heartbeat of each ground-truth sensor, one row per node, written on every
-- kernel.sensor.ping. /healthz reads it so that, with a sensor on each Kubernetes node, one live
-- node no longer hides a dead one. The Compose sensor carries no node: its row is node ''.
-- The counters are the sensor's own cumulative counts from that beat.
CREATE TABLE IF NOT EXISTS groundtruth_sensors (
    node             TEXT PRIMARY KEY,
    last_beat        TIMESTAMPTZ NOT NULL,
    observed_total   BIGINT NOT NULL DEFAULT 0,
    dropped_total    BIGINT NOT NULL DEFAULT 0,
    dropped_unmapped BIGINT NOT NULL DEFAULT 0,
    observed_by_kind JSONB
);
