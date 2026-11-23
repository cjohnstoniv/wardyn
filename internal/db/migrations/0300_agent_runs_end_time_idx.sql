-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The Runs board orders ended runs by their end time and windows them by it (store_runs_filtered.go's
-- runEndTimeSQL; a lease-ended run's end is lost_at). This index serves that order and window, so the read no
-- longer grows with the table. Built without CONCURRENTLY: an install with a large agent_runs can build it
-- CONCURRENTLY under this name before upgrading, and this statement then does nothing.
CREATE INDEX IF NOT EXISTS agent_runs_end_time_idx
    ON agent_runs ((CASE WHEN lost_reason = 'ended' THEN lost_at ELSE ended_at END) DESC, id DESC);
