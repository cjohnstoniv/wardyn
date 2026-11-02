-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- A runner exit receipt the organisation refused, on every attempt for about ten minutes, as a
-- foreign, stale or incomplete incarnation is parked: parked_at is set, a runner.exit.park audit row
-- written, and the exit sweep no longer retries it. The row is kept, and still fences its run as an
-- incomplete receipt. NULL: never parked.
ALTER TABLE runner_execution_exits ADD COLUMN IF NOT EXISTS parked_at TIMESTAMPTZ;
