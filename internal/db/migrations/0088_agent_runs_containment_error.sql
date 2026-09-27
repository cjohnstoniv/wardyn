-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Unresolved containment on a kept run (#1060, 0.8 review F06). When the
-- lease end or a lost-run cut cannot confirm the run's proxy (or agent) is
-- stopped, the run is kept rather than torn down: a teardown would remove the
-- agent container and its files on the same failing daemon. containment_error
-- is the latest stop error and containment_error_at when containment first
-- failed; the lease sweep retries the stop every pass and clears both once it
-- lands. NULL is a run whose containment is confirmed (or never attempted).
-- Metadata-only on PG11+.
ALTER TABLE agent_runs
    ADD COLUMN IF NOT EXISTS containment_error TEXT,
    ADD COLUMN IF NOT EXISTS containment_error_at TIMESTAMPTZ;
