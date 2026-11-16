-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- One row per MCP call a run proxy sent under an operation key (an approve_each call's
-- params._meta "wardyn.dev/operation-key"), written before the call is sent, so a later call with that
-- key in its namespace is answered with this send instead of being asked about and sent again. The
-- namespace is owner (the run's agent_runs.created_by) and operation_scope: corr:<the run's correlation
-- ID>, else run:<the run's id>, both read from the run row, never from what the proxy sends. sent_at is
-- the database's time of the record, before the send; answered_at is set once, when the proxy reports
-- that the server answered. Never a secret.
--
-- A row answers for its key for 24 hours from sent_at, on the database's clock (types.MCPCallSendWindow):
-- after that a read ignores it, and the key's next record overwrites it in place with the new send. No
-- row is deleted and nothing sweeps the table, and the foreign keys do not cascade, so neither the
-- approval nor the run can be deleted out from under a send; neither is deleted today. Erasing a person's run
-- tasks overwrites the scope, key and hash of their runs' rows (store.PG.BlankRunTasks), so none keeps the
-- correlation ID.
--
-- Split-role installs grant the app role SELECT, INSERT, UPDATE on it (CHANGELOG, Before you upgrade).
CREATE TABLE IF NOT EXISTS mcp_call_sends (
    owner           TEXT        NOT NULL,
    operation_scope TEXT        NOT NULL,
    operation_key   TEXT        NOT NULL,
    approval_id     UUID        NOT NULL REFERENCES approvals(id),
    run_id          UUID        NOT NULL REFERENCES agent_runs(id),
    call_sha256     TEXT        NOT NULL,
    sent_at         TIMESTAMPTZ NOT NULL,
    answered_at     TIMESTAMPTZ,
    PRIMARY KEY (owner, operation_scope, operation_key)
);
CREATE INDEX IF NOT EXISTS mcp_call_sends_approval_idx ON mcp_call_sends (approval_id);
-- Erasing a person's run tasks finds their runs' rows by run.
CREATE INDEX IF NOT EXISTS mcp_call_sends_run_idx ON mcp_call_sends (run_id);
