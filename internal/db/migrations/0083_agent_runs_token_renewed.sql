-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Lost runs (long-holds design rev 4, RL-9). token_renewed_at is when the
-- run's token was last minted: at create (the row's insert) and at every
-- renew the proxy lands (handleInternalTokenRenew). A RUNNING run whose token
-- has not been renewed for longer than the proxy keeps trying has a dead
-- identity, so the sweep marks it lost (outage) and removes its proxy.
--
-- The default is evaluated ONCE at ALTER time for the rows that already exist
-- (PG11+ stores it as the missing value; no rewrite), which gives every live
-- run at upgrade one full token life for its proxy to renew and stamp, rather
-- than reading a run older than an hour as lapsed the moment it boots. New
-- rows take their insert time, which is the create-time mint.
ALTER TABLE agent_runs
    ADD COLUMN IF NOT EXISTS token_renewed_at TIMESTAMPTZ NOT NULL DEFAULT now();
