-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Shares the AWS SSO "refused" mark across replicas and restarts.
--
-- A renewal AWS refuses with a re-auth code (invalid_request, access_denied, ...) leaves the refresh
-- token alive, so unlike a spent token (aws_sso_spent_tokens, 0068) the mark is cleared the moment
-- a renewal succeeds. Until now it lived only in one process's memory: another replica, or the same
-- one after a restart, graded the session live and passed Review until its own dispatch was refused.
--
-- fingerprint is awsSSOTokenFingerprint(refreshToken), the same one-way 8-byte digest the spent table
-- holds, so this table carries no credential material. detail is AWS's error code and its short
-- description, which the run refusal quotes (bounded to 200 bytes before it is stored). owner is the
-- secret-store namespace the credential was read from, as in aws_sso_spent_tokens. Every read is a
-- point lookup by fingerprint; marked_at backs the reaper-tick prune.
--
-- ADDITIVE ONLY: a new table with no foreign keys. A downgrade stops reading it and the in-memory
-- map alone is the old behaviour.
CREATE TABLE IF NOT EXISTS aws_sso_refused_tokens (
    fingerprint TEXT PRIMARY KEY,
    owner       TEXT NOT NULL DEFAULT '',
    detail      TEXT NOT NULL DEFAULT '',
    marked_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS aws_sso_refused_tokens_marked_at_idx ON aws_sso_refused_tokens (marked_at);
