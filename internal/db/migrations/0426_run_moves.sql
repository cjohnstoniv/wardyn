-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Moving a paused run to another machine.
--
-- run_moves records each move of a run: the source and destination placement, the pause generation it was made
-- at, its state and, for a move that did not finish, the reason. A run has at most one move in flight (the
-- partial unique index), and a move that committed holds the run until its executor releases it (released_at).
-- run_placements is the run's placement history, append only: the first move writes the run's original placement
-- (cause origin) and then its own (cause move), so the run row attests only where the run is now. An unmoved run
-- has no rows. run_sandbox_specs keeps the agent half of the run's sandbox spec, sealed by the server under the
-- run config key, so a move can create the run's sandbox again at a destination.
--
-- No credential value is stored in any of the three: run_moves and run_placements hold ids, placements and
-- reason words, and the sealed spec is recorded without the run's secret environment, its delivered secret files,
-- its proxy config and its per-run MCP token. Each table cascades with the run row, which is the audit record, and a
-- run's sealed spec is also deleted when the run goes terminal, with its proxy config.

CREATE TABLE IF NOT EXISTS run_moves (
    id                  UUID        PRIMARY KEY,
    run_id              UUID        NOT NULL REFERENCES agent_runs(id) ON DELETE CASCADE,
    state               TEXT        NOT NULL CHECK (state IN ('admitted', 'exporting', 'importing', 'committed', 'failed', 'refused')),
    pause_gen           BIGINT      NOT NULL,
    from_placement      TEXT        NOT NULL DEFAULT '',
    from_runner_id      UUID,
    from_pool_id        UUID,
    from_executor       TEXT        NOT NULL DEFAULT '',
    from_sandbox_ref    TEXT        NOT NULL DEFAULT '',
    to_placement        TEXT        NOT NULL DEFAULT '',
    to_runner_id        UUID,
    to_pool_id          UUID,
    to_executor         TEXT        NOT NULL DEFAULT '',
    to_sandbox_ref      TEXT        NOT NULL DEFAULT '',
    cleanup             TEXT        NOT NULL DEFAULT 'none' CHECK (cleanup IN ('none', 'pending', 'done')),
    source_revoke       TEXT        NOT NULL DEFAULT 'none' CHECK (source_revoke IN ('none', 'pending', 'done')),
    from_evidence_source TEXT       NOT NULL DEFAULT '' CHECK (from_evidence_source IN ('', 'substrate', 'runner_asserted')),
    to_evidence_source  TEXT        NOT NULL DEFAULT '' CHECK (to_evidence_source IN ('', 'substrate', 'runner_asserted')),
    recording_level     TEXT        NOT NULL DEFAULT '',
    from_trust_class    TEXT        NOT NULL DEFAULT '' CHECK (from_trust_class IN ('', 'personal', 'managed_device', 'org_cluster')),
    to_trust_class      TEXT        NOT NULL DEFAULT '' CHECK (to_trust_class IN ('', 'personal', 'managed_device', 'org_cluster')),
    moved_by            TEXT,
    manifest_sha256     TEXT,
    bytes               BIGINT,
    skipped             INT,
    narrowed            JSONB,
    failure             TEXT,
    to_token_jti        TEXT        NOT NULL DEFAULT '',
    heartbeat_at        TIMESTAMPTZ,
    released_at         TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    committed_at        TIMESTAMPTZ
);
CREATE UNIQUE INDEX IF NOT EXISTS run_moves_one_in_flight ON run_moves (run_id) WHERE state IN ('admitted', 'exporting', 'importing');
CREATE INDEX IF NOT EXISTS run_moves_to_runner_in_flight ON run_moves (to_runner_id) WHERE state IN ('admitted', 'exporting', 'importing');
CREATE INDEX IF NOT EXISTS run_moves_committed_held ON run_moves (run_id) WHERE state = 'committed' AND released_at IS NULL;
CREATE INDEX IF NOT EXISTS run_moves_run ON run_moves (run_id);

CREATE TABLE IF NOT EXISTS run_placements (
    run_id          UUID        NOT NULL REFERENCES agent_runs(id) ON DELETE CASCADE,
    seq             INT         NOT NULL CHECK (seq >= 1),
    cause           TEXT        NOT NULL CHECK (cause IN ('origin', 'move')),
    placement       TEXT        NOT NULL DEFAULT '',
    runner_id       UUID,
    pool_id         UUID,
    executor        TEXT        NOT NULL DEFAULT '',
    evidence_source TEXT        NOT NULL DEFAULT '' CHECK (evidence_source IN ('', 'substrate', 'runner_asserted')),
    trust_class     TEXT        NOT NULL DEFAULT '' CHECK (trust_class IN ('', 'personal', 'managed_device', 'org_cluster')),
    recording_level TEXT        NOT NULL DEFAULT '',
    move_id         UUID,
    started_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (run_id, seq)
);

CREATE TABLE IF NOT EXISTS run_sandbox_specs (
    run_id     UUID        PRIMARY KEY REFERENCES agent_runs(id) ON DELETE CASCADE,
    sealed     BYTEA       NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

DO $mig$
DECLARE
    r record;
    t text;
BEGIN
    -- Each table gets fewer verbs than the app role's default privileges hand every new table on a split-role
    -- install, so nobody but the owner keeps what a table was created with before the grants below.
    FOREACH t IN ARRAY ARRAY['run_moves', 'run_placements', 'run_sandbox_specs'] LOOP
        EXECUTE format('REVOKE ALL ON %I FROM PUBLIC', t);
        FOR r IN
            SELECT DISTINCT ro.rolname::text AS rolname
              FROM pg_catalog.pg_class c
              CROSS JOIN LATERAL pg_catalog.aclexplode(c.relacl) a
              JOIN pg_catalog.pg_roles ro ON ro.oid = a.grantee
             WHERE c.oid = t::regclass
               AND a.grantee <> c.relowner
        LOOP
            EXECUTE format('REVOKE ALL ON %I FROM %I', t, r.rolname);
        END LOOP;
    END LOOP;
    FOR r IN
        SELECT DISTINCT ro.rolname::text AS rolname
          FROM pg_catalog.pg_class c
          CROSS JOIN LATERAL pg_catalog.aclexplode(c.relacl) a
          JOIN pg_catalog.pg_roles ro ON ro.oid = a.grantee
         WHERE c.oid = 'run_attach_leases'::regclass
           AND a.privilege_type = 'DELETE' AND a.grantee <> c.relowner
    LOOP
        EXECUTE format('GRANT SELECT, INSERT, UPDATE ON run_moves TO %I', r.rolname);
        EXECUTE format('GRANT SELECT, INSERT ON run_placements TO %I', r.rolname);
        EXECUTE format('GRANT SELECT, INSERT, DELETE ON run_sandbox_specs TO %I', r.rolname);
    END LOOP;
END
$mig$;
