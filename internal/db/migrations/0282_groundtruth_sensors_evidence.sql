-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- evidence_at: the time of the latest heartbeat whose kernel event counts (observed_total plus
-- dropped_unmapped) rose over the beat before it, when the two beats were at most 15 minutes apart.
-- NULL until then. The ingest keeps beating its lifetime counts after Tetragon or its export stops,
-- so /healthz reads a sensor as healthy only while this is recent, on every replica alike.
-- Nullable with no default, so the column is metadata-only; the table's grants cover it.
ALTER TABLE groundtruth_sensors ADD COLUMN IF NOT EXISTS evidence_at TIMESTAMPTZ;
