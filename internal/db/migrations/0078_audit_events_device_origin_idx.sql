-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Looks up what the organisation already holds of one device's chain, by the
-- device's own seq (issue #102).
--
-- IngestDeviceAudit used to skip every row at or before the device's recorded
-- last_seq as a re-send. A laptop whose audit table was reset so that its seq
-- restarts (TRUNCATE ... RESTART IDENTITY, or a restore) reuses those seqs, so
-- its new rows were dropped as duplicates with no chain-reset row. A row is
-- now skipped only when the organisation's newest ingested row at that device
-- seq carries the same row_hash (store.PG.heldPrefix), which reads this
-- index.
--
-- Text, not ::bigint: an index expression that can raise would make an audit
-- insert fail on a row it cannot cast. Partial, so the organisation's own rows
-- cost it nothing. Additive: nothing rewrites audit_events.
--
-- NOT CONCURRENTLY, and it cannot be (T-41 / #701): every migration runs
-- inside a transaction (db.applyMigration, internal/db/db.go — BEGIN, the
-- migration's own SQL, the schema_migrations INSERT, COMMIT, one atomic unit
-- so a crash mid-migration never leaves a row recorded applied that is not),
-- and PostgreSQL refuses CREATE INDEX CONCURRENTLY inside a transaction block
-- outright ("cannot run inside a transaction block"). So this build takes
-- audit_events' ordinary (non-CONCURRENTLY) share lock for the duration of the
-- build: writers to OTHER tables are unaffected, and writers to audit_events
-- itself (every audited action) block until it completes. Acceptable here
-- because the index is partial (WHERE data ? 'device_origin') and additive: on
-- every deployment that predates hybrid enrolment the predicate matches zero
-- rows, so the build is a near-instant emptyset scan, not a full-table
-- rewrite. An operator with a LARGE existing audit_events table and hybrid
-- rows already in it before upgrading should expect a brief write-lock window
-- sized to that predicate's row count, not the whole table.
CREATE INDEX IF NOT EXISTS audit_events_device_origin_idx
    ON audit_events ((data->'device_origin'->>'device_id'), (data->'device_origin'->>'seq'), seq)
    WHERE data ? 'device_origin';
