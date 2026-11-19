-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- One row per metered model call: the token counts the provider's own answer reported, kept for the
-- run page and per-run totals. call_id is minted by the run's proxy for each attempt and is the key,
-- so a retried POST or a runner replaying its spool stores a call once. Numbers, a model name and a
-- host only: no content, no headers. Rows go with their run.
CREATE TABLE IF NOT EXISTS run_model_usage (
    call_id            UUID PRIMARY KEY,
    run_id             UUID NOT NULL REFERENCES agent_runs(id) ON DELETE CASCADE,
    at                 TIMESTAMPTZ NOT NULL,
    vendor             TEXT NOT NULL,
    host               TEXT NOT NULL,
    model              TEXT NOT NULL DEFAULT '',
    input_tokens       BIGINT NOT NULL DEFAULT 0,
    output_tokens      BIGINT NOT NULL DEFAULT 0,
    cache_read_tokens  BIGINT NOT NULL DEFAULT 0,
    cache_write_tokens BIGINT NOT NULL DEFAULT 0,
    reasoning_tokens   BIGINT NOT NULL DEFAULT 0,
    state              TEXT NOT NULL,
    reason             TEXT NOT NULL DEFAULT '',
    evidence_source    TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS run_model_usage_run_id_idx ON run_model_usage (run_id);
