-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Who minted the registration token a runner redeemed: the owner (a self-minted token) or an
-- operator. The owner's own view of a waiting runner needs it, since a runner registered with a
-- token made for them by someone else is the one they should be able to recognise. Empty for a
-- runner registered before this column existed.
ALTER TABLE runners ADD COLUMN IF NOT EXISTS minted_by TEXT NOT NULL DEFAULT '';
