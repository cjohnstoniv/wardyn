-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The agent a portal named as acting for a person on its token exchange (actor_token): the subject and
-- issuer of the token the identity provider issued to the portal for it. Empty on a token minted
-- without one. agent_runs.created_via_actor and attach_tickets.via_actor carry the same {sub, iss} onto
-- a run a delegated token launched and onto an attach ticket it minted; NULL when there is none.
-- Additive: an older replica neither writes nor reads these columns.
ALTER TABLE delegated_tokens ADD COLUMN IF NOT EXISTS actor_sub TEXT NOT NULL DEFAULT '';
ALTER TABLE delegated_tokens ADD COLUMN IF NOT EXISTS actor_iss TEXT NOT NULL DEFAULT '';
ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS created_via_actor JSONB;
ALTER TABLE attach_tickets ADD COLUMN IF NOT EXISTS via_actor JSONB;
