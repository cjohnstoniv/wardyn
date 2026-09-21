-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Persists the AWS SSO refresh-token "spent" mark across a daemon restart.
--
-- Before this, awssso_refresh.go marked a refresh token spent ONLY in the
-- in-memory ssoRefreshSpent map on Server — precisely when a successful
-- CreateToken redeem could not be persisted back to the secret store
-- (storeAWSSSOBlob failed) or when AWS itself retired the grant
-- (invalid_grant/expired_token/invalid_client/unauthorized_client). A
-- restart wiped that map, so a refresh token everyone already knew was dead
-- graded "renewable" again on the very next probe — the credential Wardyn
-- itself had just exchanged (and can never redeem twice) went back to
-- looking live. See internal/api/awssso_refresh.go's refreshAWSSSOBlob doc
-- comment for the persist-error arm this row is written from.
--
-- fingerprint is awsSSOTokenFingerprint(refreshToken) — 8 bytes of the
-- refresh token's SHA-256, hex-encoded (internal/api/awssso_refresh.go): the
-- SAME one-way, truncated fingerprint the in-memory map has always been
-- keyed by, so this table holds no credential material and no more of it
-- than the map already did. owner is the secret-store namespace the
-- credential was read from ("" = the operator-wide/shared credential, a
-- principal id under a per_user row) — joined to nothing else, matching the
-- map's own scoping. There is no INDEX on owner: every read of this table is
-- a point lookup by fingerprint (the PRIMARY KEY), never a scan by owner.
--
-- aws_sso_spent_tokens_marked_at_idx backs the reaper-tick prune (the SSO
-- client registration lifetime bounds how long a spent fingerprint is worth
-- remembering — see awsSSOSpentTokenRetention in awssso_refresh.go): a
-- DELETE ... WHERE marked_at < $1 with no supporting index would be a full
-- table scan on every tick.
--
-- ADDITIVE ONLY: a new table with no foreign keys into anything this
-- release did not already create. An upgrade needs no backfill; a downgrade
-- simply stops reading and writing it (the in-memory map alone is exactly
-- today's behavior).
CREATE TABLE IF NOT EXISTS aws_sso_spent_tokens (
    fingerprint TEXT PRIMARY KEY,
    owner       TEXT NOT NULL DEFAULT '',
    marked_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS aws_sso_spent_tokens_marked_at_idx ON aws_sso_spent_tokens (marked_at);
