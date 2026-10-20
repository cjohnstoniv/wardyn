-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Desktop sign-ins (#2972): a person signing in to websites inside a run's
-- desktop, and each site they shared with the run's agent. No row holds a
-- cookie or the sign-in lane credential: only the credential's SHA-256, which
-- the proxy's check matches while the sign-in is open. At most one sign-in is
-- open per run. pause_record is pause-agent's own output (the agent pids that
-- were already stopped), handed back at the resume. end_requested is set by
-- an end the runner could not finish; the sweep retries it, and end_stage is
-- the step it is stuck at, so one stuck state is audited once.
-- desktop_signin_runs is one row per run dispatched with sign-ins: the
-- sign-in image, its store and the proxy's sign-in listener. A begin on a run
-- with no row is refused before the agent is touched.
--
-- Split-role installs grant the app role SELECT, INSERT, UPDATE, DELETE on
-- the three tables (CHANGELOG, Before you upgrade).
CREATE TABLE IF NOT EXISTS desktop_signins (
    id              uuid PRIMARY KEY,
    run_id          uuid NOT NULL REFERENCES agent_runs(id) ON DELETE CASCADE,
    holder          text NOT NULL,
    credential_hash bytea NOT NULL,
    pause_record    jsonb,
    began_at        timestamptz NOT NULL,
    expires_at      timestamptz NOT NULL,
    end_requested   text,
    end_stage       text,
    ended_at        timestamptz,
    end_reason      text
);

CREATE UNIQUE INDEX IF NOT EXISTS desktop_signins_one_open ON desktop_signins (run_id) WHERE ended_at IS NULL;

CREATE TABLE IF NOT EXISTS desktop_signin_shares (
    id           uuid PRIMARY KEY,
    signin_id    uuid NOT NULL REFERENCES desktop_signins(id) ON DELETE CASCADE,
    run_id       uuid NOT NULL REFERENCES agent_runs(id) ON DELETE CASCADE,
    site         text NOT NULL,
    cookie_count integer NOT NULL,
    shared_by    text NOT NULL,
    shared_at    timestamptz NOT NULL
);

CREATE INDEX IF NOT EXISTS desktop_signin_shares_by_run ON desktop_signin_shares (run_id, shared_at);

CREATE TABLE IF NOT EXISTS desktop_signin_runs (
    run_id uuid PRIMARY KEY REFERENCES agent_runs(id) ON DELETE CASCADE
);
