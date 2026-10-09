-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Seed audit_events_legacy into the expected-partition manifest (#1809). 0111 attached the legacy
-- partition but left the manifest empty, and audit_ensure_partitions records only the partitions
-- it creates, so verify's missing-partition check never noticed an unattested removal of a legacy
-- partition that held nothing but hashless pre-0047 rows: the walk had no row to miss and the
-- manifest had no entry to miss it by.
--
-- The entry runs from MINVALUE (the text '-infinity', which casts to timestamptz and sorts before
-- every ISO timestamp the other entries carry) to the cutover. It is added unless an anchor accounts
-- for the table's absence: after a split (store.SplitLegacyAudit replaces the entry with the ranges
-- it makes) or an attested retention drop (audit_retention_drop removes the entry by name) the
-- table is gone, a 'split' anchor or a 'drop' anchor naming it exists, and so the entry does not.
-- The catalog is deliberately not asked: a database whose legacy partition was removed without an
-- anchor before this upgrade is exactly the case the entry exists to report, and 0111 always
-- attached the table. Replayable: an entry already present is left alone.

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
         WHERE NOT EXISTS (SELECT 1 FROM @ns@.audit_chain_anchors a
                            WHERE a.kind = 'split' OR (a.kind = 'drop' AND a.partition_name = 'audit_events_legacy'))
           AND NOT EXISTS (SELECT 1 FROM jsonb_array_elements(m.manifest) e WHERE e->>'name' = 'audit_events_legacy')
    $q$, '@ns@', nsq);
END
$mig$;
