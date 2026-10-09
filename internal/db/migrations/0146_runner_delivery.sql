-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- 0.9 client mode (docs/design/0.9/PLAN.md §3, §7): the runners a person registers, the actions
-- queued for a runner that is offline, and how an org-held credential reaches a local run.
-- Split-role installs grant the app role SELECT, INSERT, UPDATE, DELETE on runners,
-- runner_pending_actions and credential_delivery_policy.

-- One row per registered runner. state is the claim machine: every registration starts unclaimed
-- (a token alone proves nothing); only the owner completing the fingerprint claim makes it
-- claimed; revoked is terminal and the key is never reused. The key pair is generated on the
-- runner: only the public half is here, and no bearer exists. owner is a person's principal, so
-- these rows are personal data. posture is what the runner reported about its device, never
-- verified: posture_source says so ('runner_asserted' is the only source in 0.9).
CREATE TABLE IF NOT EXISTS runners (
    id                  UUID PRIMARY KEY,
    owner               TEXT NOT NULL,
    name                TEXT NOT NULL DEFAULT '',
    public_key          BYTEA NOT NULL CHECK (octet_length(public_key) = 32),
    key_fingerprint     TEXT NOT NULL UNIQUE,
    state               TEXT NOT NULL DEFAULT 'unclaimed' CHECK (state IN ('unclaimed', 'claimed', 'revoked')),
    version             TEXT NOT NULL DEFAULT '',
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    claimed_at          TIMESTAMPTZ,
    last_seen_at        TIMESTAMPTZ,
    revoked_at          TIMESTAMPTZ,
    posture             JSONB NOT NULL DEFAULT '{}'::jsonb,
    posture_reported_at TIMESTAMPTZ,
    posture_source      TEXT NOT NULL DEFAULT '' CHECK (posture_source IN ('', 'runner_asserted'))
);
CREATE INDEX IF NOT EXISTS runners_owner_idx ON runners (owner);

-- Actions applied first when a runner reconnects: a kill, a lease end or a proxy stop issued
-- while it was offline. Durable, so an org restart loses none. At most one unapplied action per
-- (run, kind): queueing twice is one action. A revoked runner has none (it cannot authenticate).
-- applied_at, outcome and observed_at record the runner's action_result.
CREATE TABLE IF NOT EXISTS runner_pending_actions (
    id          UUID PRIMARY KEY,
    runner_id   UUID NOT NULL REFERENCES runners(id) ON DELETE CASCADE,
    run_id      UUID NOT NULL REFERENCES agent_runs(id) ON DELETE CASCADE,
    kind        TEXT NOT NULL CHECK (kind IN ('kill', 'end', 'stop_proxy')),
    ref         TEXT NOT NULL,
    queued_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    applied_at  TIMESTAMPTZ,
    outcome     TEXT NOT NULL DEFAULT '',
    observed_at TIMESTAMPTZ
);
CREATE UNIQUE INDEX IF NOT EXISTS runner_pending_actions_one_open
    ON runner_pending_actions (run_id, kind) WHERE applied_at IS NULL;
CREATE INDEX IF NOT EXISTS runner_pending_actions_runner_open
    ON runner_pending_actions (runner_id, queued_at) WHERE applied_at IS NULL;

-- The local_credential_delivery governance document (OD-12): per credential class, how an
-- org-held credential may reach a local run. One row; absent, or a class absent from it, means
-- refuse. policy is validated at the write boundary against one closed Go definition
-- (internal/placement), the 0042 doctrine, so a new class needs no DDL.
CREATE TABLE IF NOT EXISTS credential_delivery_policy (
    singleton  BOOLEAN NOT NULL DEFAULT true CHECK (singleton),
    policy     JSONB NOT NULL DEFAULT '{"classes":{}}'::jsonb,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (singleton)
);

-- How each grant of a run reached it: '' for a run from before 0.9 and for any remote run, 'own'
-- (the person's own material), 'via_org' or 'runner_resident'.
ALTER TABLE credential_grants
    ADD COLUMN IF NOT EXISTS delivery TEXT NOT NULL DEFAULT ''
    CHECK (delivery IN ('', 'own', 'via_org', 'runner_resident'));
