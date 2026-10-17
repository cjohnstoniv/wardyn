-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- A handle names one principal-key generation without naming its owner. A
-- sealed audit field carries it (seal2.<handle>.<ciphertext>) where it used to
-- carry the person, so the row and every copy of it say whose key opens it only
-- through this table. It is random, never derived from the owner, and Destroy
-- clears it with the key, so after an erasure nothing maps a sealed field back
-- to the person. The unique index allows the NULLs of destroyed generations.
ALTER TABLE principal_keys ADD COLUMN IF NOT EXISTS handle UUID DEFAULT gen_random_uuid();
UPDATE principal_keys SET handle = NULL WHERE destroyed_at IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS principal_keys_handle ON principal_keys (handle);
