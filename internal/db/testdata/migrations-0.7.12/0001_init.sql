-- Wardyn core schema. Postgres is the ONLY required dependency.
-- Encodes the approval-mints-credential design: approvals.minted_jti is
-- written in the SAME transaction as the credential mint, after verifying
-- state = 'APPROVED' for this run+scope. audit_events is append-only.

CREATE TABLE IF NOT EXISTS agent_runs (
    id                UUID PRIMARY KEY,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by        TEXT        NOT NULL, -- human principal (sub)
    agent             TEXT        NOT NULL, -- "claude-code" | "codex-cli" | ...
    repo              TEXT        NOT NULL,
    task              TEXT        NOT NULL DEFAULT '',
    policy_id         UUID,
    confinement_class TEXT        NOT NULL CHECK (confinement_class IN ('CC1','CC2','CC3')),
    state             TEXT        NOT NULL CHECK (state IN
        ('PENDING','STARTING','RUNNING','WAITING_FOR_CONFIRMATION','STOPPED','ARCHIVED','FAILED','KILLED')),
    spiffe_id         TEXT        NOT NULL,
    runner_target     TEXT        NOT NULL CHECK (runner_target IN ('docker','k8s')),
    sandbox_ref       TEXT        NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS agent_runs_state_idx ON agent_runs (state);
CREATE INDEX IF NOT EXISTS agent_runs_created_by_idx ON agent_runs (created_by);

CREATE TABLE IF NOT EXISTS run_policies (
    id         UUID PRIMARY KEY,
    name       TEXT UNIQUE NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    spec       JSONB NOT NULL
);

-- Eligibility, NOT issuance.
CREATE TABLE IF NOT EXISTS credential_grants (
    id         UUID PRIMARY KEY,
    run_id     UUID NOT NULL REFERENCES agent_runs(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    spec       JSONB NOT NULL -- types.GrantSpec
);
CREATE INDEX IF NOT EXISTS credential_grants_run_idx ON credential_grants (run_id);

CREATE TABLE IF NOT EXISTS approvals (
    id              UUID PRIMARY KEY,
    run_id          UUID NOT NULL REFERENCES agent_runs(id) ON DELETE CASCADE,
    grant_id        UUID REFERENCES credential_grants(id) ON DELETE SET NULL,
    kind            TEXT NOT NULL CHECK (kind IN ('credential','egress_domain','tool_call')),
    requested_scope JSONB NOT NULL, -- EXACTLY what the approver saw
    state           TEXT NOT NULL DEFAULT 'PENDING'
                      CHECK (state IN ('PENDING','APPROVED','DENIED','EXPIRED')),
    requested_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    decided_at      TIMESTAMPTZ,
    decided_by      TEXT NOT NULL DEFAULT '',
    minted_jti      TEXT NOT NULL DEFAULT '', -- written back in the SAME tx as the mint
    reason          TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS approvals_run_idx ON approvals (run_id);
CREATE INDEX IF NOT EXISTS approvals_state_idx ON approvals (state) WHERE state = 'PENDING';

-- Append-only audit log: the system of record. UPDATE/DELETE are blocked.
CREATE TABLE IF NOT EXISTS audit_events (
    seq        BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    id         UUID NOT NULL,
    time       TIMESTAMPTZ NOT NULL DEFAULT now(),
    run_id     UUID,
    actor_type TEXT NOT NULL CHECK (actor_type IN ('human','agent','system')),
    actor      TEXT NOT NULL,
    action     TEXT NOT NULL,
    target     TEXT NOT NULL DEFAULT '',
    outcome    TEXT NOT NULL CHECK (outcome IN ('success','failure','denied')),
    source_ip  TEXT NOT NULL DEFAULT '',
    data       JSONB
);
CREATE INDEX IF NOT EXISTS audit_events_run_idx ON audit_events (run_id);
CREATE INDEX IF NOT EXISTS audit_events_time_idx ON audit_events (time);

CREATE OR REPLACE FUNCTION audit_events_append_only() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'audit_events is append-only';
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS audit_events_no_update ON audit_events;
CREATE TRIGGER audit_events_no_update
    BEFORE UPDATE OR DELETE ON audit_events
    FOR EACH ROW EXECUTE FUNCTION audit_events_append_only();

-- Default SecretStore: age-encrypted values.
CREATE TABLE IF NOT EXISTS secrets (
    name       TEXT PRIMARY KEY,
    ciphertext BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Embedded identity provider revocation denylist (kill-switch cascade).
CREATE TABLE IF NOT EXISTS identity_revocations (
    jti        TEXT PRIMARY KEY,
    run_id     UUID,
    revoked_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS identity_revocations_run_idx ON identity_revocations (run_id);
