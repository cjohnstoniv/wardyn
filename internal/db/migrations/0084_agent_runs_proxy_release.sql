-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Proxy-only revive and restart with current limits (long-holds design rev 4,
-- RL-10). proxy_release is the wardynd release that started the run's current
-- proxy sidecar: written with the sandbox ref at dispatch, and again whenever
-- a revive or restart replaces the proxy. The admin version-window listing
-- reads it to find the runs whose proxy is older than wardynd N-1, which is
-- as far back as the internal API is kept compatible.
--
-- '' is a proxy started before this column existed, a release the listing
-- cannot place, so it lists it as outside the window. Metadata-only on PG11+.
ALTER TABLE agent_runs
    ADD COLUMN IF NOT EXISTS proxy_release TEXT NOT NULL DEFAULT '';
