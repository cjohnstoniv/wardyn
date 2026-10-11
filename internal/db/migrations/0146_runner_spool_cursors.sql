-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Audit retention must not erase runner replay deduplication. One stream is
-- permanently bound to its run and runner; neither a cursor reset nor another
-- spool identity can rewrite an acknowledged sequence. The recent hashes cover
-- the shared replay batch, including a crash before the local cursor fsync.
-- Run or runner erasure removes these metadata-only receipts. Audit retention
-- remains independent; no decision bodies or credential material live here.
CREATE TABLE IF NOT EXISTS runner_spool_cursors (
    run_id UUID PRIMARY KEY REFERENCES agent_runs(id) ON DELETE CASCADE,
    runner_id UUID NOT NULL REFERENCES runners(id) ON DELETE CASCADE,
    spool_id UUID NOT NULL,
    last_seq BIGINT NOT NULL DEFAULT 0 CHECK (last_seq >= 0),
    recent_hashes JSONB NOT NULL DEFAULT '{}'::jsonb,
    overflow_end NUMERIC(20,0) NOT NULL DEFAULT 0 CHECK (overflow_end >= 0),
    overflow_hash TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS runner_spool_cursors_runner_idx ON runner_spool_cursors (runner_id);
