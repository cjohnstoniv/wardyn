-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Revocation during and after an outage (#4083).
--
-- run_github_token_seals keeps each GitHub installation token a run minted, so that a background worker can hand it
-- back to GitHub (DELETE /installation/token, which must be presented the token itself) after the run's revoke,
-- without the revoke waiting for GitHub. The value is sealed with kek.Seal under the 'cred' subject key of the run's
-- masking-manifest owner (run_mask_manifest.owner, 0109), the AAD binding the run id, the jti and the key version, so
-- destroying that key leaves the row undecryptable and a person's erasure deletes the owner's rows. A writer re-reads
-- the run's manifest FOR SHARE in the mint's own transaction, so no row is written after an erasure fenced it. A run
-- with no masking manifest keeps no copy. revoke_due_at is set once a revoke asked for the token, next_attempt_at is
-- the worker's next try and its claim hold. A row that ends (handed back, expired or abandoned) has its sealed copy
-- emptied and stays as a tombstone until 5 minutes after the token's expiry, so a later revoke of the run reads what
-- happened.
--
-- run_local_containment records that a local operator contained a run on its runner (wardyn-runnerd contain), as the
-- runner reported it in a STATE page. It only ever grows: a run's row goes with the run.
--
-- The app role is granted both tables here, found from the catalog and never by name: every role but the owner that
-- holds UPDATE on agent_runs gets SELECT, INSERT, UPDATE, DELETE on run_github_token_seals and SELECT, INSERT on
-- run_local_containment (ON CONFLICT DO NOTHING needs no UPDATE; a run's row goes with the run through ON DELETE
-- CASCADE, which runs as the table's owner).
CREATE TABLE run_github_token_seals (
    run_id          uuid NOT NULL REFERENCES agent_runs(id) ON DELETE CASCADE,
    jti             text NOT NULL,
    owner           text NOT NULL,
    key_version     integer NOT NULL,
    sealed          bytea NOT NULL,
    expires_at      timestamptz NOT NULL,
    revoke_due_at   timestamptz,
    next_attempt_at timestamptz,
    attempts        integer NOT NULL DEFAULT 0,
    ended_at        timestamptz,
    ended_outcome   text CHECK (ended_outcome IN ('handed_back', 'expired', 'abandoned')),
    PRIMARY KEY (run_id, jti),
    CHECK ((ended_at IS NULL) = (ended_outcome IS NULL))
);

CREATE INDEX run_github_token_seals_expires_idx ON run_github_token_seals (expires_at);
CREATE INDEX run_github_token_seals_due_idx ON run_github_token_seals (next_attempt_at)
    WHERE revoke_due_at IS NOT NULL AND ended_at IS NULL;

CREATE TABLE run_local_containment (
    run_id       uuid PRIMARY KEY REFERENCES agent_runs(id) ON DELETE CASCADE,
    runner_id    uuid NOT NULL,
    contained_at timestamptz NOT NULL,
    scope        text NOT NULL CHECK (scope IN ('run', 'all')),
    operator_uid integer NOT NULL,
    reported_at  timestamptz NOT NULL DEFAULT now()
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
        EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON run_github_token_seals TO %I', r.rolname);
        EXECUTE format('GRANT SELECT, INSERT ON run_local_containment TO %I', r.rolname);
    END LOOP;
END
$mig$;
