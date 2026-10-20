-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The Recordings list asks every cast part for its byte size and last 8 KiB (recording.PGStore.StatAndTail),
-- and computing the tail decompresses the whole stored value, so a page of runs with 32 MiB parts detoasted
-- gigabytes. SaveCast now stores both beside the payload; StatAndTail reads them when stat_updated_at equals
-- updated_at, i.e. when they were written with the payload now in the row (a save by a binary from before
-- this migration moves updated_at and leaves stat_updated_at behind, and that row is read from its payload).
-- The backfill reads every stored recording once.
ALTER TABLE recordings
    ADD COLUMN IF NOT EXISTS size_bytes BIGINT,
    ADD COLUMN IF NOT EXISTS tail BYTEA,
    ADD COLUMN IF NOT EXISTS stat_updated_at TIMESTAMPTZ;

UPDATE recordings
   SET size_bytes = octet_length(payload),
       tail = substring(payload from greatest(octet_length(payload) - 8192 + 1, 1) for 8192),
       stat_updated_at = updated_at
 WHERE stat_updated_at IS NULL;
