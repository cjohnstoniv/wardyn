-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- A revision on the organisation's runner-pool defaults (0.9.3): it starts at 1 and rises by one on every
-- write to the one defaults row, so a console that read the document can name what it read and a writer
-- that raced another administrator is refused instead of silently replacing their change.
ALTER TABLE runner_pool_org_defaults ADD COLUMN IF NOT EXISTS revision BIGINT NOT NULL DEFAULT 1;
