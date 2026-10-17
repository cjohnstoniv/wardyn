-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The last mask_gen cursor that erased each owner. No FK or retention: an
-- empty erase and a deleted/recreated person must still fence old work.
-- Writers and erasers take mask_gen first, so the comparison and the values
-- commit in the same order. Split-role installs grant SELECT, INSERT, UPDATE.
CREATE TABLE IF NOT EXISTS mask_owner_erasures (
    owner TEXT PRIMARY KEY CHECK (owner <> ''),
    gen BIGINT NOT NULL CHECK (gen > 0)
);
