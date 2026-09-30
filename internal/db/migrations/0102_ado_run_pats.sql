-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The personal access tokens Wardyn creates for `minted_pat` runs on Azure
-- DevOps, one row per token (#1428). A run can hold several live tokens:
-- renewal and widening create a new one and leave the old to expire at
-- valid_to; run end revokes every live one.
--
-- The row is written BEFORE the token is first used, so a crash between
-- create and use still leaves a record for the sweep to revoke. It holds no
-- secret: the token value lives in daemon memory only.
--
-- owner / provider_row_id / org: whose token, on which provider row and for
-- which organisation, so a disconnect or a row change can find every live
-- token it makes stale. scope is the space-joined scope string the token was
-- created with. valid_to is when Azure DevOps expires it on its own.
--
-- revoked_at NULL is a token still live at Azure DevOps as far as Wardyn knows;
-- it is set when the token is revoked or its valid_to has passed.
-- revoke_reason is a closed Go vocabulary (api/ado_pat_contract.go), so no
-- CHECK backs it (the 0042 doctrine). last_error is the last failed revoke
-- attempt, '' otherwise: a row with last_error and revoked_at NULL is a token
-- Wardyn could not revoke, which the setup check lists while valid_to is
-- ahead. No FK to agent_runs: the row must outlive a pruned
-- run long enough to be revoked.
CREATE TABLE IF NOT EXISTS ado_run_pats (
    run_id           UUID        NOT NULL,
    authorization_id UUID        NOT NULL,
    owner            TEXT        NOT NULL,
    provider_row_id  TEXT        NOT NULL,
    org              TEXT        NOT NULL,
    scope            TEXT        NOT NULL,
    valid_to         TIMESTAMPTZ NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at       TIMESTAMPTZ,
    revoke_reason    TEXT        NOT NULL DEFAULT '',
    last_error       TEXT        NOT NULL DEFAULT '',
    PRIMARY KEY (run_id, authorization_id)
);

-- The sweep and the disconnect path read what is still live, never the history.
CREATE INDEX IF NOT EXISTS ado_run_pats_unrevoked_idx
    ON ado_run_pats (owner, run_id) WHERE revoked_at IS NULL;
