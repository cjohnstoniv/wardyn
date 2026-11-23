-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Snapshots of Linux VM sandboxes and the forks made from them, and the facts a snapshot's secrets check reads.
-- New tables only: agent_runs is not altered. No table here stores a credential value.
--
-- machine_snapshots: one row per snapshot of a run's disk, kept on the runner that made it (runner_id, with no ON
-- DELETE, as kept_machines has: the layer stays on the runner whatever happens to the row's runner). The row names
-- the image the layer backs onto (image_ref, image_digest, image_id), the machine the run had (vm_machine), the
-- run's unit and workspaces, the layer's file name, size and digest, and the base file's stamp (inode, size, mtime
-- and ctime). source_run_id is cleared when the run row goes: a layer never depends on its run. failure is the
-- reason a failed row failed; layer_gone_at is when the runner confirmed it holds no layer for a failed row. At most
-- one snapshot of a run is being made at once.
--
-- machine_snapshot_holds: one row per fork whose disk backs onto a snapshot's layer. A held snapshot's row cannot
-- be deleted (ON DELETE RESTRICT), so the layer is never deleted while a fork uses it.
--
-- run_fork_sources: which snapshot, and which run, a fork was made from.
--
-- run_dispatch_secret_facts: what the run's dispatch (and every later revive) decided about two credentials that
-- have no row of their own: an AWS sign-in for Bedrock whose role credentials live in the sandbox, and a git
-- token the sandbox's helper holds. recorded_jti is the id of the run token the last writer gave the run's proxy,
-- never the token. A run with no row can never be snapshotted.
--
-- The app role is granted the tables here, found from the catalog and never by name: every role but the owner
-- that holds DELETE on run_attach_leases gets SELECT, INSERT, UPDATE and DELETE on machine_snapshots, SELECT,
-- INSERT and DELETE on machine_snapshot_holds, SELECT and INSERT on run_fork_sources, and SELECT, INSERT and
-- UPDATE on run_dispatch_secret_facts.
CREATE TABLE IF NOT EXISTS machine_snapshots (
    id            UUID        PRIMARY KEY,
    source_run_id UUID        REFERENCES agent_runs(id) ON DELETE SET NULL,
    owner_subject TEXT        NOT NULL CHECK (owner_subject <> ''),
    runner_id     UUID        NOT NULL REFERENCES runners(id),
    machine_kind  TEXT        NOT NULL CHECK (machine_kind = 'linux_vm'),
    image_ref     TEXT        NOT NULL,
    image_digest  TEXT        NOT NULL DEFAULT '',
    image_id      TEXT        NOT NULL,
    vm_machine    JSONB       NOT NULL,
    layer_name    TEXT        NOT NULL,
    layer_sha256  TEXT,
    bytes         BIGINT      CHECK (bytes IS NULL OR bytes >= 0),
    base_stamp    TEXT,
    unit_id       UUID,
    workspace_ids UUID[]      NOT NULL DEFAULT '{}',
    state         TEXT        NOT NULL CHECK (state IN ('creating', 'ready', 'deleting', 'failed')),
    failure       TEXT,
    label         TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at  TIMESTAMPTZ,
    expires_at    TIMESTAMPTZ,
    cut_start     TIMESTAMPTZ,
    cut_end       TIMESTAMPTZ,
    layer_gone_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS machine_snapshots_sweep ON machine_snapshots (state, created_at)
    WHERE state = 'creating' OR (state = 'failed' AND layer_gone_at IS NULL);
CREATE INDEX IF NOT EXISTS machine_snapshots_owner ON machine_snapshots (owner_subject) WHERE state <> 'deleting';
CREATE INDEX IF NOT EXISTS machine_snapshots_runner_image ON machine_snapshots (runner_id, image_id);
CREATE UNIQUE INDEX IF NOT EXISTS machine_snapshots_one_creating ON machine_snapshots (source_run_id) WHERE state = 'creating';

CREATE TABLE IF NOT EXISTS machine_snapshot_holds (
    snapshot_id UUID        NOT NULL REFERENCES machine_snapshots(id) ON DELETE RESTRICT,
    run_id      UUID        NOT NULL REFERENCES agent_runs(id) ON DELETE CASCADE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (snapshot_id, run_id)
);

CREATE INDEX IF NOT EXISTS machine_snapshot_holds_run ON machine_snapshot_holds (run_id);

CREATE TABLE IF NOT EXISTS run_fork_sources (
    run_id        UUID        PRIMARY KEY REFERENCES agent_runs(id) ON DELETE CASCADE,
    snapshot_id   UUID        REFERENCES machine_snapshots(id) ON DELETE SET NULL,
    source_run_id UUID,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS run_dispatch_secret_facts (
    run_id             UUID        PRIMARY KEY REFERENCES agent_runs(id) ON DELETE CASCADE,
    bedrock_resident   BOOLEAN     NOT NULL,
    git_token_resident BOOLEAN     NOT NULL,
    recorded_jti       TEXT        NOT NULL DEFAULT '',
    recorded_at        TIMESTAMPTZ NOT NULL DEFAULT now()
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
         WHERE c.oid = 'run_attach_leases'::regclass
           AND a.privilege_type = 'DELETE' AND a.grantee <> c.relowner
    LOOP
        EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON machine_snapshots TO %I', r.rolname);
        EXECUTE format('GRANT SELECT, INSERT, DELETE ON machine_snapshot_holds TO %I', r.rolname);
        EXECUTE format('GRANT SELECT, INSERT ON run_fork_sources TO %I', r.rolname);
        EXECUTE format('GRANT SELECT, INSERT, UPDATE ON run_dispatch_secret_facts TO %I', r.rolname);
    END LOOP;
END
$mig$;
