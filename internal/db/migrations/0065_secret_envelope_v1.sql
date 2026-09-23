-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Envelope v1 for stored credentials (credential-storage design §2.2). Until
-- now a `secrets` row was one age payload with no associated data: a database
-- writer could move a ciphertext to another (owned_by, name) undetected, and
-- anyone holding the PUBLIC age recipient could forge a row. A v1 row seals the
-- value with AES-256-GCM under its own data key (DEK), bound to the row's
-- (owned_by, name), and stores the DEK wrapped by a key-encryption key (KEK).
--
--   enc_version  1 = envelope v1. The DEFAULT stays 0 (legacy age) on purpose:
--                every existing row becomes v0 here, wardynd converts them all
--                at boot (secretstore/pg ConvertV0), and a row an OLDER wardynd
--                inserts afterwards lands as v0, which a v1 read refuses by
--                name ("an older wardynd is still writing") instead of misreading.
--   kek_id       which KEK wrapped the DEK ("local:<recipient fp>" today); a
--                read refuses a kek_id it is not configured with. '' on v0.
--   wrapped_dek  KEK.Wrap(DEK, owner/name). Empty on v0.
--   ciphertext   (existing column) nonce(12) || AES-256-GCM(DEK, value) on v1.
--   last_used_at stamped by the injection sink (a later lane); NULL until used.
--   expires_at   the latest time a stored sign-in can still be used or renewed
--                (a later lane); NULL for keys and tokens.
ALTER TABLE secrets ADD COLUMN IF NOT EXISTS enc_version SMALLINT NOT NULL DEFAULT 0;
ALTER TABLE secrets ADD COLUMN IF NOT EXISTS kek_id TEXT NOT NULL DEFAULT '';
ALTER TABLE secrets ADD COLUMN IF NOT EXISTS wrapped_dek BYTEA NOT NULL DEFAULT ''::bytea;
ALTER TABLE secrets ADD COLUMN IF NOT EXISTS last_used_at TIMESTAMPTZ NULL;
ALTER TABLE secrets ADD COLUMN IF NOT EXISTS expires_at TIMESTAMPTZ NULL;
