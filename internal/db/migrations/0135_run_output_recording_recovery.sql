-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Recovery claims and recording-only erasure share the run-output lock.
ALTER TABLE run_outputs DROP CONSTRAINT run_outputs_source_check;
ALTER TABLE run_outputs ADD CONSTRAINT run_outputs_source_check
    CHECK (source IN ('stdout', 'pane_snapshot', 'recording'));

-- No run FK: a recording-source erasure survives deletion and identifier reuse.
-- An unfenced orphan is removable housekeeping; an erasure is never swept.
-- Split-role installs grant the app role SELECT, INSERT, UPDATE on this table.
CREATE TABLE IF NOT EXISTS run_output_recording_recovery (
    run_id UUID PRIMARY KEY,
    erased_at TIMESTAMPTZ,
    requested_generation BIGINT NOT NULL DEFAULT 0,
    completed_generation BIGINT NOT NULL DEFAULT 0,
    claim_token UUID,
    claimed_at TIMESTAMPTZ,
    CHECK (requested_generation >= completed_generation AND completed_generation >= 0),
    CHECK (claim_token IS NULL OR claimed_at IS NOT NULL)
);
CREATE INDEX IF NOT EXISTS run_output_recording_pending_idx
    ON run_output_recording_recovery (claimed_at NULLS FIRST, run_id)
    WHERE erased_at IS NULL AND requested_generation > completed_generation;
