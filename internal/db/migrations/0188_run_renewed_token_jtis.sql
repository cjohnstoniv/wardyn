-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The ids of the two tokens a run's proxy last held across a renewal (0.9.3): the one the renewal minted and
-- the one it presented. A revive replaces the proxy and revokes the token it was started with, but a token the
-- replaced proxy renewed since stays valid until its own expiry; these two ids are what the revive revokes.
ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS token_jti TEXT, ADD COLUMN IF NOT EXISTS token_prev_jti TEXT;
