-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- run_mcp_sandboxes: one row per MCP server of a run that starts from a command, which runs in a sandbox of
-- its own beside the run's. The row is the server's state (starting, ready, restarting, stopped, failed), the
-- cause of its last stop or failure, its restart window, the digest of its sealed policy, the CPU and memory
-- it holds, its proxy's sealed config, the address the agent's proxy reaches it on and the SHA-256 of the
-- relay token, never the token, and the jti of its proxy's control-plane token (and the previous one, kept
-- until it lapses, so the rows that proxy flushes as it is replaced still land). Its key is the run and the
-- server's component ordinal; the run_id prefix serves every read of a run's rows. A run's rows go with it.
--
-- The app role is granted the table here, found from the catalog and never by name: every role but the owner
-- that holds UPDATE on agent_runs, which the application writes, gets SELECT, INSERT, UPDATE and DELETE on
-- run_mcp_sandboxes.
CREATE TABLE IF NOT EXISTS run_mcp_sandboxes (
    run_id               UUID        NOT NULL REFERENCES agent_runs (id) ON DELETE CASCADE,
    ordinal              INTEGER     NOT NULL,
    server_key           TEXT        NOT NULL,
    sandbox_ref          TEXT        NOT NULL DEFAULT '',
    state                TEXT        NOT NULL CHECK (state IN ('starting', 'ready', 'restarting', 'stopped', 'failed')),
    restarts             INTEGER     NOT NULL DEFAULT 0,
    restart_window_start TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_cause           TEXT,
    policy_digest        TEXT        NOT NULL DEFAULT '',
    reserved             JSONB,
    proxy_config_sealed  BYTEA,
    relay_addr           TEXT        NOT NULL DEFAULT '',
    relay_token_sha256   TEXT        NOT NULL DEFAULT '',
    cp_token_jti         TEXT        NOT NULL DEFAULT '',
    prev_cp_token_jti    TEXT,
    prev_cp_token_until  TIMESTAMPTZ,
    grant_ids            UUID[]      NOT NULL DEFAULT '{}',
    stopped_at           TIMESTAMPTZ,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (run_id, ordinal)
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
         WHERE c.oid = 'agent_runs'::regclass
           AND a.privilege_type = 'UPDATE' AND a.grantee <> c.relowner
    LOOP
        EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON run_mcp_sandboxes TO %I', r.rolname);
    END LOOP;
END
$mig$;
