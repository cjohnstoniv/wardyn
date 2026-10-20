-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- What a runner last reported about its own health, one row per runner, overwritten (types.RunnerView):
--   offline_summary  why it could not connect or start since its session before (types.RunnerOfflineSummary),
--                    NULL until a runner that sends one has; offline_summary_at is when it was stored,
--                    which spaces the writes
--   relay_status     its last run relay check (types.RunnerRelayStatus), NULL for a runner with no relay
-- Both are runner_asserted. The size checks are the store's caps again. A split migrator and app role
-- install grants the app role SELECT, INSERT, UPDATE.
CREATE TABLE IF NOT EXISTS runner_health (
    runner_id          UUID PRIMARY KEY REFERENCES runners (id) ON DELETE CASCADE,
    offline_summary    JSONB CHECK (offline_summary IS NULL OR octet_length(offline_summary::text) <= 2048),
    offline_summary_at TIMESTAMPTZ,
    relay_status       JSONB CHECK (relay_status IS NULL OR octet_length(relay_status::text) <= 2048)
);
