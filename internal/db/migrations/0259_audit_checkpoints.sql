-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Signed audit checkpoints. wardynd re-hashes the audit rows appended since the last checkpoint, folds their
-- row_hash values and signs one statement over them with a key the database cannot read. Each row here is one
-- checkpoint: the exact signed bytes, the signature and the key id. A database actor can still rewrite audit
-- rows, but cannot sign a checkpoint over the rewrite, so a rewrite of any checkpointed row shows offline.
--
-- The table is append-only like audit_events, owner included (the same trigger function), and it is written
-- by exactly the roles that may call audit_append, found from the catalog and never by name: INSERT and SELECT
-- only, whatever default privileges granted. A row a database actor inserts carries no valid signature, and
-- the signer refuses to continue past it.
--
-- Created in the audit log's own schema and every name schema-qualified, as the chain migrations do.

DO $mig$
DECLARE
    ns   text;
    r    record;
BEGIN
    SELECT n.nspname INTO ns
      FROM pg_catalog.pg_class c
      JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
     WHERE c.oid = 'audit_events'::regclass;

    EXECUTE format($t$
CREATE TABLE IF NOT EXISTS %1$I.audit_checkpoints (
    seq_lo     BIGINT PRIMARY KEY,
    seq_hi     BIGINT NOT NULL UNIQUE CHECK (seq_hi >= seq_lo),
    kid        TEXT NOT NULL,
    statement  TEXT NOT NULL,
    signature  BYTEA NOT NULL CHECK (octet_length(signature) = 64),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- No two checkpoints share a seq: a range inside or across a stored one conflicts, not only one sharing
    -- its seq_lo or seq_hi. Range types carry their own GiST operator class, so no extension is needed.
    EXCLUDE USING gist (int8range(seq_lo, seq_hi, '[]') WITH &&)
)$t$, ns);

    EXECUTE format('DROP TRIGGER IF EXISTS audit_checkpoints_no_update ON %I.audit_checkpoints', ns);
    EXECUTE format('CREATE TRIGGER audit_checkpoints_no_update BEFORE UPDATE OR DELETE ON %1$I.audit_checkpoints FOR EACH ROW EXECUTE FUNCTION %1$I.audit_events_append_only()', ns);
    EXECUTE format('DROP TRIGGER IF EXISTS audit_checkpoints_no_truncate ON %I.audit_checkpoints', ns);
    EXECUTE format('CREATE TRIGGER audit_checkpoints_no_truncate BEFORE TRUNCATE ON %1$I.audit_checkpoints FOR EACH STATEMENT EXECUTE FUNCTION %1$I.audit_events_append_only()', ns);

    -- Nobody but the owner keeps anything the table was created with: a split-role install's ALTER DEFAULT
    -- PRIVILEGES hands the app role UPDATE and DELETE on every new table, which this one must not have.
    EXECUTE format('REVOKE ALL ON %I.audit_checkpoints FROM PUBLIC', ns);
    FOR r IN
        SELECT DISTINCT ro.rolname::text AS rolname
          FROM pg_catalog.pg_class c
          CROSS JOIN LATERAL pg_catalog.aclexplode(c.relacl) a
          JOIN pg_catalog.pg_roles ro ON ro.oid = a.grantee
         WHERE c.oid = format('%I.audit_checkpoints', ns)::regclass
           AND a.grantee <> c.relowner
    LOOP
        EXECUTE format('REVOKE ALL ON %I.audit_checkpoints FROM %I', ns, r.rolname);
    END LOOP;
    FOR r IN
        SELECT ro.rolname::text AS rolname
          FROM pg_catalog.pg_proc p
          CROSS JOIN LATERAL pg_catalog.aclexplode(p.proacl) a
          JOIN pg_catalog.pg_roles ro ON ro.oid = a.grantee
         WHERE p.oid = format('%I.audit_append(uuid, timestamptz, uuid, text, text, text, text, text, text, jsonb)', ns)::regprocedure
           AND a.privilege_type = 'EXECUTE' AND a.grantee <> p.proowner
    LOOP
        EXECUTE format('GRANT INSERT, SELECT ON %I.audit_checkpoints TO %I', ns, r.rolname);
    END LOOP;
END
$mig$;
