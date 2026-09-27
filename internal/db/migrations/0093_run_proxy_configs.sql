-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- run_proxy_configs holds each run's rendered proxy config (#1176): the run
-- token, the per-run MITM CA key and the upstream-proxy credential among it.
-- It is sealed by wardynd (AES-256-GCM under the wardyn-run-config-key boot
-- key, which the secret store keeps under its key-encryption key) and bound to
-- its run, so the database never holds it in the clear. It exists so a revive
-- can rebuild a run's proxy without reading it back from the proxy container,
-- which therefore holds no config at rest. Deleted when the run goes terminal;
-- the cascade covers a deleted run.
CREATE TABLE IF NOT EXISTS run_proxy_configs (
    run_id     UUID PRIMARY KEY REFERENCES agent_runs(id) ON DELETE CASCADE,
    sealed     BYTEA NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
