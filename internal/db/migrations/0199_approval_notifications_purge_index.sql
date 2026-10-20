-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- notify.Worker.Tick purges terminal rows older than the retention window on every 15 s tick of every
-- replica, even when nothing was due. The only index on approval_notifications is partial on the two live
-- states, so the purge's `state IN ('sent','cancelled','dead') AND last_attempt_at < ...` scanned the table.
-- Partial on the terminal states, so it holds only the rows the purge reads.
CREATE INDEX IF NOT EXISTS approval_notifications_purge_idx
    ON approval_notifications (last_attempt_at) WHERE state IN ('sent','cancelled','dead');
