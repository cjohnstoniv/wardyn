-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- store.PG.RunHeldContainedOnRunner and store.PG.LastRunnerTeardown read a run's whole action history,
-- applied rows included, and run under the run's op lock on every lease send. Every earlier index on this
-- table is partial on applied_at IS NULL (or on settled_at), which neither query's predicate implies, so
-- each read scanned the table, and the cost grew with all-time runner history rather than with live runs.
-- Not partial, so it serves both queries and the correlated NOT EXISTS probe on kind.
CREATE INDEX IF NOT EXISTS runner_pending_actions_run_kind_queued
    ON runner_pending_actions (run_id, kind, queued_at);
