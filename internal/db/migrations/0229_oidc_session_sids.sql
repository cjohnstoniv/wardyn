-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The identity provider's session id (the ID token's sid) of each console sign-in, for OpenID
-- Connect Front-Channel Logout (WARDYN_OIDC_FRONTCHANNEL_LOGOUT). The provider's logout request
-- names only iss and sid, and console sessions are stateless cookies, so this is the one place a
-- sid leads back to the person whose sessions it ends. Written at sign-in only while the setting is
-- on; a logout consumes its row; the reaper tick prunes rows older than the longest session.
--
-- A split-role install grants the app role SELECT, INSERT, UPDATE, DELETE on oidc_session_sids
-- (docs/OPERATIONS.md).
--
-- ADDITIVE ONLY: a new table with no foreign keys. Re-runnable: every statement is IF NOT EXISTS.
CREATE TABLE IF NOT EXISTS oidc_session_sids (
    issuer       TEXT NOT NULL,
    sid          TEXT NOT NULL,
    sub          TEXT NOT NULL,
    signed_in_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (issuer, sid)
);

CREATE INDEX IF NOT EXISTS oidc_session_sids_signed_in_at_idx ON oidc_session_sids (signed_in_at);
