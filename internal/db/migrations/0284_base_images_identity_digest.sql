-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- base_images_identity_idx (0031) kept the whole steps JSONB in a btree key. A btree entry is capped near
-- 2.7 KB, so a custom image whose steps were a few KB of text that does not compress failed the insert with
-- SQLSTATE 54000 and the create answered 500. The identity is now a sha256 of the steps' jsonb binary-send
-- form, as 0213 did for approval scopes: jsonb stores keys normalized, so equal steps hash equal, and the
-- entry is a fixed 32 bytes. NULL and an empty array stay one identity: both COALESCE to '[]'. Steps are a
-- list of strings the server writes, so the number spellings 0213 notes do not arise.
-- The new index is built before the old one is dropped, so uniqueness holds throughout, and the existing
-- rows already satisfy it: rows the old index kept apart have different values, so different bytes.
CREATE UNIQUE INDEX IF NOT EXISTS base_images_identity_digest_idx
    ON base_images (kind, image, sha256(jsonb_send(COALESCE(steps, '[]'::jsonb))));

DROP INDEX IF EXISTS base_images_identity_idx;
