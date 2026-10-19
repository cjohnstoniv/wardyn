-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- last_rejoined_at is when a held request last re-entered the hold on a still-PENDING row: a retried
-- push or egress connection, or a raise that deduplicated onto the row. approval.Hold measures a
-- bounded hold from the later of requested_at and this, so a retry that is held again reads as held.
-- NULL until the first rejoin.
ALTER TABLE approvals ADD COLUMN IF NOT EXISTS last_rejoined_at TIMESTAMPTZ;
