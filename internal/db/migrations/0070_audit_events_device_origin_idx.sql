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
-- seq carries the same row_hash (store.PG.heldDeviceRows), which reads this
-- index.
--
-- Text, not ::bigint: an index expression that can raise would make an audit
-- insert fail on a row it cannot cast. Partial, so the organisation's own rows
-- cost it nothing. Additive: nothing rewrites audit_events.
CREATE INDEX IF NOT EXISTS audit_events_device_origin_idx
    ON audit_events ((data->'device_origin'->>'device_id'), (data->'device_origin'->>'seq'), seq)
    WHERE data ? 'device_origin';
