-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Portals with their own identity issuer. Additive only: nothing here changes what a console-issuer
-- portal, a delegated token or an existing identity row means.
--
--   * delegates gains the issuer-portal columns. A row with issuer = '' is a console portal, as before.
--   * portal_issuers is the registry of issuer bindings: one row per issuer spelling, naming the internal
--     address and the claim that names a person. It is deleted only by the retirement of an unused binding.
--   * console_issuers records every console issuer a deployment has booted with. Rows are only added.
--   * portal_grants holds the grants an issuer portal hands out. It is a table of its own, so an older
--     server's lookup of a delegated token never reads one.
--   * principal_identities gains the portal person's display email and the one-time introduction event, and
--     a trigger that keeps the portal lifecycle (a deadline, a reactivation, a tombstone) behind a marker
--     that only the portal's own store writes set.
--
-- No secret and no token is stored here: a grant is held by the hash of its bearer, as delegated_tokens is.

ALTER TABLE delegates
    ADD COLUMN IF NOT EXISTS issuer                         TEXT    NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS internal_issuer                TEXT    NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS issuer_key                     TEXT    NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS token_profile                  TEXT    NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS groups_claim                   TEXT    NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS email_domains                  TEXT[]  NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS user_type                      TEXT    NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS ui_session_renewal             BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS audience                       TEXT,
    ADD COLUMN IF NOT EXISTS required_scopes                TEXT[],
    ADD COLUMN IF NOT EXISTS max_subject_token_age_seconds  INT,
    ADD COLUMN IF NOT EXISTS grant_ttl_seconds              INT,
    ADD COLUMN IF NOT EXISTS required_claim                 JSONB,
    ADD COLUMN IF NOT EXISTS portal_managed_admission       BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS admission_rev                  BIGINT  NOT NULL DEFAULT 1;

-- A console portal has always had a group: the API refuses an empty one. A row written by hand might not,
-- and the admission rule below would then fail with a bare constraint error, so name the count and the
-- remedy first.
DO $mig$
DECLARE
    n bigint;
BEGIN
    SELECT count(*) INTO n FROM delegates WHERE issuer = '' AND scope_group = '';
    IF n > 0 THEN
        RAISE EXCEPTION 'migration 0294: % portal row(s) in delegates have an empty scope_group and no issuer; set a group on each (or revoke the portal and delete the row) before upgrading', n;
    END IF;
END
$mig$;

-- Each constraint is dropped before it is added, so the migration can run again over a catalogue that has it.
-- A missing maximum age means its default (300) and a missing grant lifetime means the maximum age, so each
-- bound is checked on the effective value: a NULL bound would make the comparison unknown, which a CHECK passes.
ALTER TABLE delegates
    DROP CONSTRAINT IF EXISTS delegates_max_subject_token_age,
    ADD CONSTRAINT delegates_max_subject_token_age CHECK (
        issuer = '' OR COALESCE(max_subject_token_age_seconds, 300) BETWEEN 300 AND 600);
ALTER TABLE delegates
    DROP CONSTRAINT IF EXISTS delegates_grant_ttl,
    ADD CONSTRAINT delegates_grant_ttl CHECK (
        issuer = '' OR COALESCE(grant_ttl_seconds, COALESCE(max_subject_token_age_seconds, 300))
                       BETWEEN 300 AND COALESCE(max_subject_token_age_seconds, 300));
ALTER TABLE delegates
    DROP CONSTRAINT IF EXISTS delegates_required_claim_shape,
    ADD CONSTRAINT delegates_required_claim_shape CHECK (
        required_claim IS NULL OR (
            jsonb_typeof(required_claim) = 'object'
            AND required_claim ?& ARRAY['claim', 'value']
            AND required_claim - 'claim' - 'value' = '{}'::jsonb
            AND jsonb_typeof(required_claim -> 'claim') = 'string'
            AND jsonb_typeof(required_claim -> 'value') = 'string'));
-- A console portal never carries the flag, and an issuer portal has a group exactly when the flag is off.
ALTER TABLE delegates
    DROP CONSTRAINT IF EXISTS delegates_admission_shape,
    ADD CONSTRAINT delegates_admission_shape CHECK (
        (issuer = '' AND scope_group <> '' AND NOT portal_managed_admission)
        OR (issuer <> '' AND (scope_group = '') = portal_managed_admission));

CREATE TABLE IF NOT EXISTS portal_issuers (
    issuer_key           TEXT        PRIMARY KEY,
    issuer               TEXT        NOT NULL UNIQUE,
    internal_issuer_norm TEXT        NOT NULL,
    person_id_claim      TEXT        NOT NULL CHECK (person_id_claim IN ('uid', 'sub')),
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by           TEXT        NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS console_issuers (
    issuer_norm              TEXT        PRIMARY KEY,
    issuer                   TEXT        NOT NULL,
    first_booted_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    first_booted_by_replica  TEXT        NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS portal_grants (
    id                UUID        PRIMARY KEY,
    delegate_id       UUID        NOT NULL REFERENCES delegates (id),
    token_sha256      TEXT        NOT NULL UNIQUE,
    principal         TEXT        NOT NULL,
    user_type         TEXT        NOT NULL,
    chain_id          UUID        NOT NULL,
    subject_issued_at TIMESTAMPTZ NOT NULL,
    rev_seq           BIGINT      NOT NULL DEFAULT 0,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at        TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS portal_grants_chain ON portal_grants (delegate_id, principal, chain_id, expires_at);
CREATE INDEX IF NOT EXISTS portal_grants_expires_at ON portal_grants (expires_at);

-- Which portals a person has used: one row per (person, portal), written when a grant is minted for them. It
-- is the person's portal history, kept apart from the grants (which expire and are cleared on suspension).
-- A purge or an erasure of the person deletes their rows; an expiry sweep and a suspension do not.
CREATE TABLE IF NOT EXISTS portal_person_portals (
    identity_id   UUID        NOT NULL REFERENCES principal_identities (id) ON DELETE CASCADE,
    delegate_id   UUID        NOT NULL REFERENCES delegates (id),
    first_used_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (identity_id, delegate_id)
);

ALTER TABLE principal_identities
    ADD COLUMN IF NOT EXISTS display_email            TEXT        NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS introduced_by            UUID,
    ADD COLUMN IF NOT EXISTS introduced_issuer        TEXT        NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS introduction_event_id    UUID,
    ADD COLUMN IF NOT EXISTS introduction_event_at    TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS introduction_audited_at  TIMESTAMPTZ;

-- The portal sweep's re-emit of introductions that were committed and never marked delivered.
CREATE INDEX IF NOT EXISTS principal_identities_introduction_pending
    ON principal_identities (introduction_event_at)
    WHERE introduction_event_id IS NOT NULL AND introduction_audited_at IS NULL;

-- The portal lifecycle fence. A portal person's identity row has an issuer that starts 'delegate:person:'.
-- "Marked" means the transaction ran SET LOCAL wardyn.portal_lifecycle = 'v1', which only the portal's own
-- suspension, reactivation and purge writes do. The body branches on TG_OP because OLD is unassigned on an
-- insert.
--   Insert, always: the row starts clean (no deactivation, no deadline, no tombstone, no email_lower).
--   Update, unless marked: a change to purge_after, a reactivation and a tombstone are refused.
--   Update, always: clearing a tombstone, a tombstone that keeps a display email, and a non-empty
--   email_lower are refused.
--   Allowed unmarked: setting deactivated_at, authority_epoch changes, and updates that leave the
--   guarded columns alone.
-- It binds whatever SQL any binary sends, an older SCIM worker included.
DROP FUNCTION IF EXISTS principal_identities_portal_lifecycle() CASCADE;
CREATE FUNCTION principal_identities_portal_lifecycle() RETURNS trigger
LANGUAGE plpgsql
AS $body$
DECLARE
    marked boolean := coalesce(pg_catalog.current_setting('wardyn.portal_lifecycle', true), '') = 'v1';
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NOT starts_with(NEW.issuer, 'delegate:person:') THEN
            RETURN NEW;
        END IF;
        IF NEW.deactivated_at IS NOT NULL OR NEW.purge_after IS NOT NULL OR NEW.purged_at IS NOT NULL
           OR NEW.email_lower <> '' THEN
            RAISE EXCEPTION 'a portal person starts with no deactivation, deadline, tombstone or email'
                USING ERRCODE = 'check_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF NOT (starts_with(OLD.issuer, 'delegate:person:') OR starts_with(NEW.issuer, 'delegate:person:')) THEN
        RETURN NEW;
    END IF;
    IF OLD.purged_at IS NOT NULL AND NEW.purged_at IS NULL THEN
        RAISE EXCEPTION 'a portal person''s tombstone is permanent' USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.purged_at IS NOT NULL AND NEW.display_email <> '' THEN
        RAISE EXCEPTION 'a purged portal person keeps no display email' USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.email_lower <> '' THEN
        RAISE EXCEPTION 'a portal person keeps no email_lower' USING ERRCODE = 'check_violation';
    END IF;
    IF NOT marked THEN
        IF NEW.purge_after IS DISTINCT FROM OLD.purge_after THEN
            RAISE EXCEPTION 'a portal person''s purge deadline is set only by the portal lifecycle'
                USING ERRCODE = 'check_violation';
        END IF;
        IF OLD.deactivated_at IS NOT NULL AND NEW.deactivated_at IS NULL THEN
            RAISE EXCEPTION 'a portal person is reactivated only by the portal lifecycle'
                USING ERRCODE = 'check_violation';
        END IF;
        IF OLD.purged_at IS NULL AND NEW.purged_at IS NOT NULL THEN
            RAISE EXCEPTION 'a portal person is purged only by the portal lifecycle'
                USING ERRCODE = 'check_violation';
        END IF;
    END IF;
    RETURN NEW;
END;
$body$;

CREATE TRIGGER principal_identities_portal_lifecycle
    BEFORE INSERT OR UPDATE ON principal_identities
    FOR EACH ROW EXECUTE FUNCTION principal_identities_portal_lifecycle();

-- The app role is granted the new tables here, found from the catalog and never by name: every role but the
-- owner that holds UPDATE on delegates, which the portal's revoke and admission writes use. The exchange's
-- FOR KEY SHARE on a binding and the retirement's FOR UPDATE need UPDATE on portal_issuers, and the retirement
-- deletes the row; the grants sweep deletes portal_grants; a purge or an erasure deletes portal_person_portals rows; console_issuers is only read and added to.
DO $mig$
DECLARE
    r record;
BEGIN
    FOR r IN
        SELECT DISTINCT ro.rolname::text AS rolname
          FROM pg_catalog.pg_class c
          CROSS JOIN LATERAL pg_catalog.aclexplode(c.relacl) a
          JOIN pg_catalog.pg_roles ro ON ro.oid = a.grantee
         WHERE c.oid = 'delegates'::regclass
           AND a.privilege_type = 'UPDATE' AND a.grantee <> c.relowner
    LOOP
        EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON portal_issuers TO %I', r.rolname);
        EXECUTE format('GRANT SELECT, INSERT ON console_issuers TO %I', r.rolname);
        EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON portal_grants TO %I', r.rolname);
        EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON portal_person_portals TO %I', r.rolname);
    END LOOP;
END
$mig$;
