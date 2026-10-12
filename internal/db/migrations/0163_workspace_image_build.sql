-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The record of what built a workspace's image (#1516): written with
-- image_ref by one compare-and-swap; NULL for every existing row, which reads
-- as unrecorded; nothing is backfilled.
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS image_build jsonb;
