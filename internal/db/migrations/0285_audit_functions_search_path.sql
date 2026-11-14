-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Takes the install's schema off the search_path of the audit functions and of 0176's two revocation
-- functions. Each pinned pg_catalog, then that schema, then pg_temp. PostgreSQL weighs every function
-- of a called name on the path alike, so a role that may CREATE in that schema could add one whose
-- parameter types fit a call more exactly than pg_catalog's (text for a quoted literal where nextval
-- takes regclass; a typed parameter where format takes VARIADIC "any"), and it ran as the calling
-- function's owner. The path is now pg_catalog then pg_temp. The audit functions' bodies already name
-- every Wardyn object with the schema read when they were created, so nothing they call moves; the two
-- revocation functions named their sequence bare and are re-created naming it in full.
--
-- Only the twelve functions Wardyn ships there that run as their owner or set a search_path of their
-- own are changed, each named by its exact signature, and only where the migrator owns it. Any other
-- function or procedure, an operator's own named audit_* included, and any of the twelve the migrator
-- does not own, is left as it is, so none of them can fail this migration; the boot check reads them
-- as before. Idempotent. db.replayTriggerMigrations, which restores the chain trigger from the files
-- that define it, puts this path back on the chain function after them.

DO $mig$
DECLARE
    ns  text;
    ons text;
    r   record;
BEGIN
    SELECT n.nspname INTO ns
      FROM pg_catalog.pg_class c
      JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
     WHERE c.oid = 'audit_events'::regclass;
    SELECT n.nspname INTO ons
      FROM pg_catalog.pg_class c
      JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
     WHERE c.oid = 'oidc_session_revocations'::regclass;

    FOR r IN
        SELECT p.oid::pg_catalog.regprocedure AS fn, p.proname::text AS fname
          FROM pg_catalog.unnest(ARRAY[
                   pg_catalog.format('%I.oidc_revocation_stamp()', ons),
                   pg_catalog.format('%I.oidc_revocation_position()', ons),
                   pg_catalog.format('%I.audit_events_chain()', ns),
                   pg_catalog.format('%I.audit_append(uuid, timestamptz, uuid, text, text, text, text, text, text, jsonb)', ns),
                   pg_catalog.format('%I.audit_ensure_partitions(integer)', ns),
                   pg_catalog.format('%I.audit_partition_digest(text)', ns),
                   pg_catalog.format('%I.audit_retention_set_policy(integer)', ns),
                   pg_catalog.format('%I.audit_retention_drop(text, text, text, text)', ns),
                   pg_catalog.format('%I.audit_harden_relation(regclass, text[])', ns),
                   pg_catalog.format('%I.audit_retention_partitions(text, boolean)', ns),
                   pg_catalog.format('%I.audit_retention_window()', ns),
                   pg_catalog.format('%I.audit_retention_refuse(text, text)', ns)]) AS s(sig)
          JOIN pg_catalog.pg_proc p ON p.oid = pg_catalog.to_regprocedure(s.sig)
         WHERE pg_catalog.pg_has_role(current_user, p.proowner, 'USAGE')
    LOOP
        IF r.fname = 'oidc_revocation_stamp' THEN
            EXECUTE pg_catalog.format($fn$
CREATE OR REPLACE FUNCTION %1$I.oidc_revocation_stamp() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
AS $body$
BEGIN
    NEW.seq := nextval(%2$L::regclass);
    RETURN NEW;
END;
$body$
$fn$, ons, pg_catalog.format('%I.oidc_revocation_seq', ons));
        ELSIF r.fname = 'oidc_revocation_position' THEN
            EXECUTE pg_catalog.format($fn$
CREATE OR REPLACE FUNCTION %1$I.oidc_revocation_position() RETURNS bigint
LANGUAGE sql
SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
AS $body$ SELECT last_value FROM %1$I.oidc_revocation_seq $body$
$fn$, ons);
        ELSE
            EXECUTE pg_catalog.format('ALTER FUNCTION %s SET search_path = pg_catalog, pg_temp', r.fn);
        END IF;
    END LOOP;
END
$mig$;
