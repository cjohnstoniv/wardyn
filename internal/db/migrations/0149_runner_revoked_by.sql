-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Who revoked a runner and why. An owner sees that an administrator revoked
-- their runner, and an owner's "not mine" revoke tells the security team that
-- someone registered a computer with a token made for that person. Both are
-- written once, by the revoke that ends the runner. Empty for a runner revoked
-- before these columns existed.
ALTER TABLE runners
    ADD COLUMN IF NOT EXISTS revoked_by    TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS revoke_reason TEXT NOT NULL DEFAULT '',
    DROP CONSTRAINT IF EXISTS runners_revoke_reason,
    ADD CONSTRAINT runners_revoke_reason CHECK (revoke_reason IN ('', 'not_mine'));
