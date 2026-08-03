-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Postgres-backed recording store (internal/recording/pgstore.go), closing the
-- "fs recordings" HA residual: FSStore's directory is per-pod, so a cast
-- written by the replica that dispatched a run 404s on replay if the request
-- lands on a different replica. This table backs the store that becomes the
-- DEFAULT (see cmd/wardynd/boot_flags.go WARDYN_RECORDING_STORE); setting
-- WARDYN_RECORDING_STORE=fs still selects the old on-disk behavior.
--
-- cast_key uses the SAME addressing FSStore does (internal/recording/store.go
-- CastKey): a bare run id for a batch-run cast, or "<runID>~<suffix>" for an
-- interactive attach session, so the two stores are drop-in compatible from
-- every caller's point of view and OpenCast resolves either form identically.
--
-- payload is BYTEA, not jsonb/text, following migration 0026's precedent: an
-- asciicast is an opaque, pre-masking byte stream that can legitimately carry
-- NUL/non-UTF-8 bytes, and text/jsonb would reject or mangle those instead of
-- round-tripping them.
--
-- updated_at (bumped on every SaveCast/SaveCastNamed upsert, not just
-- created_at) is what PGStore.Sweep filters on, mirroring FSStore.Sweep's
-- mtime-based age measurement — a cast that was re-saved stays fresh even if
-- first created long ago.
CREATE TABLE IF NOT EXISTS recordings (
    cast_key   TEXT        PRIMARY KEY,
    payload    BYTEA       NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Serves PGStore.Sweep's `WHERE updated_at < now() - $1::interval` retention
-- scan. Recordings default to keep-forever (WARDYN_RECORDING_RETENTION_DAYS=0),
-- so the table grows without bound in the common case; this keeps a configured
-- retention window a cheap index scan instead of a full table scan.
CREATE INDEX IF NOT EXISTS recordings_updated_at_idx ON recordings (updated_at);
