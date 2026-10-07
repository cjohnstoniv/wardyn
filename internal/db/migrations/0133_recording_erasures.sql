-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- A fence outlives recordings, retention and run history. No foreign key or
-- retention sweep may remove it: late uploads must remain refused after restart.
-- TEXT preserves the recording Store's arbitrary-key contract (0028).
-- Split-role installs grant the app role SELECT, INSERT on recording_erasures.
CREATE TABLE IF NOT EXISTS recording_erasures (
    run_id TEXT PRIMARY KEY,
    erased_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
