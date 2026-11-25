-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- A runner's labels, one row per runner and source (types.RunnerLabelSource):
--   runner_asserted  what the runner proposed at registration; shown, never trusted
--   operator         what a security operator wrote; the only set run.create, the run read, the audit export
--                    and placement rules read
-- labels is a JSON object of Kubernetes-syntax labels, at most 16 (types.ValidateRunnerLabels checks the syntax;
-- the size check here is a backstop). created_by is "runner:<id>" for runner_asserted, else the operator.
-- A revoke leaves the rows (a revoke is an UPDATE of runners); a revoked runner takes no run, so they are inert.
-- A write replaces the source's whole set.
--
-- ADDITIVE ONLY: a new table. A downgrade stops reading it.
CREATE TABLE IF NOT EXISTS runner_labels (
    runner_id  UUID NOT NULL REFERENCES runners (id) ON DELETE CASCADE,
    source     TEXT NOT NULL CHECK (source IN ('runner_asserted', 'operator')),
    labels     JSONB NOT NULL CHECK (jsonb_typeof(labels) = 'object' AND octet_length(labels::text) <= 8192),
    created_by TEXT NOT NULL CHECK (created_by <> ''),
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (runner_id, source)
);

-- The app role is granted the table here, found from the catalog and never by name: every role but the owner
-- that holds UPDATE on runners gets SELECT, INSERT, UPDATE and DELETE on runner_labels.
DO $mig$
DECLARE
    r record;
BEGIN
    FOR r IN
        SELECT DISTINCT ro.rolname::text AS rolname
          FROM pg_catalog.pg_class c
          CROSS JOIN LATERAL pg_catalog.aclexplode(c.relacl) a
          JOIN pg_catalog.pg_roles ro ON ro.oid = a.grantee
         WHERE c.oid = 'runners'::regclass
           AND a.privilege_type = 'UPDATE' AND a.grantee <> c.relowner
    LOOP
        EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON runner_labels TO %I', r.rolname);
    END LOOP;
END
$mig$;
