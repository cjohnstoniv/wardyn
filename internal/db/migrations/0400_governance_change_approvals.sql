-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Approvals from more than one person on a held governance change.
--
-- governance_changes gains required_approvals (how many distinct people must approve, 1 to 9, fixed when the
-- change is proposed and never lowered after) and require_security (at least one counted approver must hold the
-- security tier). Every change written before this reads required_approvals 1 and require_security false, so it
-- is decided as it always was.
--
-- governance_change_approvals holds one row for each person who has approved a still-pending change. approver is
-- the principal and approver_email the email it carried; tier and unit_scope are what the approval was given as
-- (display and audit only). api_token_id and api_token_kind name the token an approval was given with ('api',
-- 'cli_access' or 'cli_admin'), cli_session_id the session of a CLI token, and rev_seq is the revocation
-- sequence read when the approval was recorded. The row stores no role and no groups: whether an approval still
-- counts is worked out again from the person's latest sign-in record. dropped names why an approval stopped
-- counting (dropped_at when it is known); the row is kept, and approving again replaces it. One row for each
-- (change, approver). An approver is never blank: a blank principal reads as the admin token.
--
-- governance_approval_counts holds the number of approvals a held change of one kind needs, with the default
-- row at target_kind '*'. It is empty until a settings route writes it.
--
-- principal_groups gains roles, the roles claim of the person's last sign-in, kept apart from groups so the
-- 2048-byte cap on the group list cannot push a role out. NULL for every row written before this: those rows'
-- groups already carry the claim.
--
-- The app role is granted the new tables here, found from the catalog and never by name: every role but the
-- owner that holds DELETE on governance_changes gets SELECT, INSERT, UPDATE and DELETE on both. The new columns
-- ride the grants their tables already have. Never a secret.
ALTER TABLE governance_changes
    ADD COLUMN IF NOT EXISTS required_approvals INT NOT NULL DEFAULT 1 CHECK (required_approvals BETWEEN 1 AND 9),
    ADD COLUMN IF NOT EXISTS require_security BOOLEAN NOT NULL DEFAULT false;

ALTER TABLE principal_groups ADD COLUMN IF NOT EXISTS roles JSONB;

CREATE TABLE IF NOT EXISTS governance_change_approvals (
    change_id      UUID        NOT NULL REFERENCES governance_changes(id) ON DELETE CASCADE,
    approver       TEXT        NOT NULL CHECK (approver <> ''),
    approver_email TEXT        NOT NULL DEFAULT '',
    tier           TEXT        NOT NULL,
    unit_scope     TEXT        NOT NULL DEFAULT '',
    reason         TEXT        NOT NULL DEFAULT '',
    approved_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    api_token_id   UUID,
    api_token_kind TEXT,
    cli_session_id UUID,
    rev_seq        BIGINT      NOT NULL DEFAULT 0,
    dropped        TEXT        NOT NULL DEFAULT '',
    dropped_at     TIMESTAMPTZ,
    PRIMARY KEY (change_id, approver)
);

CREATE TABLE IF NOT EXISTS governance_approval_counts (
    target_kind      TEXT        PRIMARY KEY,
    required         INT         NOT NULL CHECK (required BETWEEN 1 AND 9),
    require_security BOOLEAN     NOT NULL DEFAULT false,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

DO $mig$
DECLARE
    r record;
BEGIN
    FOR r IN
        SELECT DISTINCT ro.rolname::text AS rolname
          FROM pg_catalog.pg_class c
          CROSS JOIN LATERAL pg_catalog.aclexplode(c.relacl) a
          JOIN pg_catalog.pg_roles ro ON ro.oid = a.grantee
         WHERE c.oid = 'governance_changes'::regclass
           AND a.privilege_type = 'DELETE' AND a.grantee <> c.relowner
    LOOP
        EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON governance_change_approvals TO %I', r.rolname);
        EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON governance_approval_counts TO %I', r.rolname);
    END LOOP;
END
$mig$;
