-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- run_outputs holds each run's captured output: one row per run, masked before it is inserted. A row
-- with captured_at NULL is pending (a capture is owed: the dispatching process holds the tail and will
-- write the final row, and the retention sweeper resolves a pending row whose claim went stale).
-- output is BYTEA because a byte tail can begin mid-character and may hold NUL bytes, which TEXT
-- refuses. source is 'stdout' for a run's own output. mask_scope says whether the writer held the
-- run's complete masking manifest for the whole capture ('run') or not ('globals_only').
--
-- run_output_erasures is the erasure fence: a tombstone per run whose output was erased. Every writer
-- and reader of run_outputs checks it in its own transaction, so no replica can recreate or serve a
-- row after the erasure. Both tables follow the run's own deletion.
CREATE TABLE IF NOT EXISTS run_outputs (
    run_id      UUID PRIMARY KEY REFERENCES agent_runs(id) ON DELETE CASCADE,
    output      BYTEA NOT NULL DEFAULT '\x',
    truncated   BOOLEAN NOT NULL DEFAULT false,
    incomplete  BOOLEAN NOT NULL DEFAULT false,
    capture_gap BOOLEAN NOT NULL DEFAULT false,
    source      TEXT NOT NULL CHECK (source IN ('stdout', 'pane_snapshot')),
    mask_scope  TEXT CHECK (mask_scope IN ('run', 'globals_only')),
    captured_at TIMESTAMPTZ,
    claimed_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS run_outputs_captured_at_idx ON run_outputs (captured_at) WHERE captured_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS run_outputs_pending_idx ON run_outputs (claimed_at) WHERE captured_at IS NULL;

CREATE TABLE IF NOT EXISTS run_output_erasures (
    run_id    UUID PRIMARY KEY REFERENCES agent_runs(id) ON DELETE CASCADE,
    erased_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
