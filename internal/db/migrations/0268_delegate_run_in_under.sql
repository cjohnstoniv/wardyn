-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The governance unit a portal is pinned to (POST /api/v1/admin/delegates run_in_under): a run it
-- launches must resolve to this unit or one below it. NULL, the default, is an unpinned portal.
-- No foreign key on purpose: if the unit is deleted the pin still refuses, it never drops to unpinned.
ALTER TABLE delegates ADD COLUMN IF NOT EXISTS run_in_under UUID;
