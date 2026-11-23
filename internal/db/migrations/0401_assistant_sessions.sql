-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- assistant_sessions: one row for each conversation a person holds with the assistant. A session runs inside a
-- governed run of its own (run_id) and acts as its person: principal, email, role, user_type, groups and
-- groups_truncated are what the person's request published when the session began, the snapshot every tool call
-- narrows again. purpose, the target and the serving binding (provider_id, provider_uid, model) are fixed at
-- start. A repository draft holds its session in fetching before the run exists. One run holds at most one live
-- session, and one person at most one live session for each purpose. draft is the working draft, person_letters
-- the letters the transcript uses for people's names, preview_digest the digest of the records the person was
-- shown. An erasure blanks the person's fields; the retention sweep deletes an ended session's events, clears
-- draft, person_letters, groups and email, and keeps the rest as the audit record.
--
-- assistant_session_events: the session's transcript, numbered by seq within the session (a reused run's new
-- session continues the count after the old one's last).
CREATE TABLE IF NOT EXISTS assistant_sessions (
    id               UUID PRIMARY KEY,
    run_id           UUID NULL REFERENCES agent_runs (id) ON DELETE CASCADE,
    principal        TEXT NOT NULL,
    email            TEXT NOT NULL DEFAULT '',
    role             TEXT NOT NULL DEFAULT '',
    user_type        TEXT NOT NULL DEFAULT '',
    groups           TEXT[] NOT NULL DEFAULT '{}',
    groups_truncated BOOLEAN NOT NULL DEFAULT false,
    purpose          TEXT NOT NULL,
    target_kind      TEXT NOT NULL,
    target_key       TEXT NOT NULL DEFAULT '',
    workspace_id     UUID NULL,
    repo             TEXT NULL,
    provider_id      TEXT NOT NULL,
    provider_uid     TEXT NOT NULL DEFAULT '',
    model            TEXT NOT NULL,
    state            TEXT NOT NULL CHECK (state IN ('fetching', 'starting', 'ready', 'ended')),
    draft            JSONB NULL,
    person_letters   JSONB NULL,
    preview_digest   TEXT NOT NULL DEFAULT '',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_turn_at     TIMESTAMPTZ NULL,
    ended_at         TIMESTAMPTZ NULL,
    end_reason       TEXT NULL,
    CHECK (run_id IS NOT NULL OR state IN ('fetching', 'ended'))
);

CREATE UNIQUE INDEX IF NOT EXISTS assistant_sessions_one_live_per_run
    ON assistant_sessions (run_id) WHERE state <> 'ended';
CREATE UNIQUE INDEX IF NOT EXISTS assistant_sessions_one_live_per_person
    ON assistant_sessions (principal, purpose) WHERE state <> 'ended';

CREATE TABLE IF NOT EXISTS assistant_session_events (
    session_id UUID NOT NULL REFERENCES assistant_sessions (id) ON DELETE CASCADE,
    seq        BIGINT NOT NULL,
    kind       TEXT NOT NULL,
    payload    JSONB NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (session_id, seq)
);

-- Split-role installs: the roles that write run_attach_leases write these too.
DO $mig$
DECLARE
    r record;
BEGIN
    FOR r IN
        SELECT DISTINCT ro.rolname::text AS rolname
          FROM pg_catalog.pg_class c
          CROSS JOIN LATERAL pg_catalog.aclexplode(c.relacl) a
          JOIN pg_catalog.pg_roles ro ON ro.oid = a.grantee
         WHERE c.oid = 'run_attach_leases'::regclass
           AND a.privilege_type = 'DELETE' AND a.grantee <> c.relowner
    LOOP
        EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON assistant_sessions TO %I', r.rolname);
        EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON assistant_session_events TO %I', r.rolname);
    END LOOP;
END
$mig$;
