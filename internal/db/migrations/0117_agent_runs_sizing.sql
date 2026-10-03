-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The configured reservation a run was dispatched with (0.8.6 fleet capacity view):
-- the agent's effective CPU/memory requests and limits, the wardyn-proxy envelope and the
-- runner kind, exactly the values the driver applied. Written once at dispatch by a scoped
-- UPDATE (SetRunSizing), never by CreateRun and never re-derived: a later change to the
-- deployment's default size, request ratio or proxy envelope must not move a recorded run.
--
-- No defaults and no backfill: NULL means the run predates the record (or the best-effort
-- write failed). runner_kind is separate from runner_target, which is never NULL, so a
-- legacy row cannot look recorded. proxy_cpu_millis NULL with runner_kind set means the
-- proxy had no CPU cap.
ALTER TABLE agent_runs
    ADD COLUMN IF NOT EXISTS runner_kind              TEXT,
    ADD COLUMN IF NOT EXISTS agent_cpu_request_millis INTEGER,
    ADD COLUMN IF NOT EXISTS agent_cpu_limit_millis   INTEGER,
    ADD COLUMN IF NOT EXISTS agent_memory_request_mib INTEGER,
    ADD COLUMN IF NOT EXISTS agent_memory_limit_mib   INTEGER,
    ADD COLUMN IF NOT EXISTS proxy_cpu_millis         INTEGER,
    ADD COLUMN IF NOT EXISTS proxy_memory_mib         INTEGER;
