-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Seed audit_events_legacy into the expected-partition manifest (#1809). 0111 attached the legacy
-- partition but left the manifest empty, and audit_ensure_partitions records only the partitions
-- it creates, so verify's missing-partition check never noticed an unattested removal of a legacy
-- partition that held nothing but hashless pre-0047 rows: the walk had no row to miss and the
-- manifest had no entry to miss it by.
--
-- The entry runs from MINVALUE (the text '-infinity', which casts to timestamptz and sorts before
-- every ISO timestamp the other entries carry) to the cutover. It is added only while the legacy
-- table is still a partition of the log: after a split (store.SplitLegacyAudit replaces the entry
-- with the ranges it makes) or an attested retention drop (audit_retention_drop removes the entry
-- by name) the table is gone and so is its entry. Replayable: an entry already present is left alone.

DO $mig$
DECLARE
    ns  text;
    nsq text;
BEGIN
    SELECT n.nspname INTO ns
      FROM pg_catalog.pg_class c
      JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
     WHERE c.oid = 'audit_events'::regclass;
    nsq := quote_ident(ns);

    EXECUTE replace($q$
        UPDATE @ns@.audit_partition_meta m
           SET manifest = jsonb_build_array(jsonb_build_object(
                   'name', 'audit_events_legacy',
                   'lo', '-infinity',
                   'hi', to_char(m.cutover AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'))) || m.manifest
         WHERE EXISTS (SELECT 1
                         FROM pg_catalog.pg_inherits i
                         JOIN pg_catalog.pg_class c ON c.oid = i.inhrelid
                        WHERE i.inhparent = '@ns@.audit_events'::regclass
                          AND c.oid = to_regclass('@ns@.audit_events_legacy'))
           AND NOT EXISTS (SELECT 1 FROM jsonb_array_elements(m.manifest) e WHERE e->>'name' = 'audit_events_legacy')
    $q$, '@ns@', nsq);
END
$mig$;
