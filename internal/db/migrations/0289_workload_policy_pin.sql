-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- policy_pin (0241's workloads): the launch policy digest every launch of the workload must resolve to,
-- '' for none. Only the pin routes write it; an apply, a restart and a launch never do. A launch whose
-- policy resolves to another digest is refused before any run is made, so the current run keeps running.
-- NOT NULL with a constant default, so the column is metadata-only; the check scans the table once, and
-- the table's grants cover it.
ALTER TABLE workloads ADD COLUMN IF NOT EXISTS policy_pin TEXT NOT NULL DEFAULT ''
    CHECK (policy_pin = '' OR policy_pin ~ '^[0-9a-f]{64}$');
