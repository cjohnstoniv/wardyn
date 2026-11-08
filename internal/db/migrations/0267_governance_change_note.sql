-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The proposer's note on a governance change: why it is wanted (note_reason) and an optional ticket
-- reference (note_ticket). The existing reason column is the decider's reason for a rejection, so the
-- note has its own columns. Both default to empty, so a change nobody wrote a note on reads as before.
-- The proposer's personal fields erasure clears them with the rest of the proposer's row.
ALTER TABLE governance_changes
    ADD COLUMN IF NOT EXISTS note_reason TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS note_ticket TEXT NOT NULL DEFAULT '';
