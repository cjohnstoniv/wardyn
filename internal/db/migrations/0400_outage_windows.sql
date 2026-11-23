-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Recorded outage windows (#4082): when the control plane could not be reached, when a runner's link was down, and when
-- the server's own proxies stopped renewing as a group. Written only by the control plane, on the database's clock.
CREATE TABLE cp_heartbeat (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    beat_at   timestamptz NOT NULL
);
INSERT INTO cp_heartbeat (singleton, beat_at) VALUES (true, now());

CREATE TABLE outage_windows (
    id         bigserial PRIMARY KEY,
    kind       text NOT NULL CHECK (kind IN ('cp_outage', 'runner_link', 'server_outage')),
    runner_id  uuid,
    started_at timestamptz NOT NULL,
    ended_at   timestamptz,
    detail     jsonb NOT NULL DEFAULT '{}',
    CHECK ((kind = 'runner_link') = (runner_id IS NOT NULL)),
    CHECK (ended_at IS NULL OR ended_at >= started_at),
    CHECK (kind <> 'cp_outage' OR ended_at IS NOT NULL)
);
CREATE INDEX outage_windows_kind_runner_started ON outage_windows (kind, runner_id, started_at);
CREATE UNIQUE INDEX outage_windows_one_open ON outage_windows
    (kind, COALESCE(runner_id, '00000000-0000-0000-0000-000000000000'::uuid)) WHERE ended_at IS NULL;
