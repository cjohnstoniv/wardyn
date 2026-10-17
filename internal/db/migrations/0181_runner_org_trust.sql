-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Organization-owned runners and the per-runner trust class (0.9.3).
--
-- Ownership is a column, never a reserved principal: an org runner has owner_kind 'org' and an empty
-- owner, so no equality check on owner can hand a person an org runner. trust_class is written by the
-- claim and afterwards only by a security operator (PUT /runners/{id}/trust); nothing the runner sends,
-- its posture included, feeds it. org_cluster is the class of an org runner and only of one.
-- registration_token_id names the token a runner redeemed, so the org claim can compare its claimer
-- with the token's minter. Existing rows become person / personal.
ALTER TABLE runners
    ADD COLUMN IF NOT EXISTS owner_kind            TEXT NOT NULL DEFAULT 'person',
    ADD COLUMN IF NOT EXISTS trust_class           TEXT NOT NULL DEFAULT 'personal',
    ADD COLUMN IF NOT EXISTS trust_set_by          TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS trust_set_at          TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS registration_token_id UUID,
    DROP CONSTRAINT IF EXISTS runners_owner_kind,
    ADD CONSTRAINT runners_owner_kind CHECK (owner_kind IN ('person', 'org')),
    DROP CONSTRAINT IF EXISTS runners_org_owner_empty,
    ADD CONSTRAINT runners_org_owner_empty CHECK (owner_kind = 'person' OR owner = ''),
    DROP CONSTRAINT IF EXISTS runners_trust_class,
    ADD CONSTRAINT runners_trust_class CHECK (trust_class IN ('personal', 'managed_device', 'org_cluster')),
    DROP CONSTRAINT IF EXISTS runners_trust_class_coupling,
    ADD CONSTRAINT runners_trust_class_coupling CHECK ((trust_class = 'org_cluster') = (owner_kind = 'org'));

-- minted_by_kind says whether a human or the shared admin token minted the token; break_glass marks an
-- admin-token mint of an org token while WARDYN_GOVERNANCE_SECOND_HUMAN is on.
ALTER TABLE runner_registration_tokens
    ADD COLUMN IF NOT EXISTS owner_kind      TEXT NOT NULL DEFAULT 'person',
    ADD COLUMN IF NOT EXISTS minted_by_kind  TEXT NOT NULL DEFAULT 'human',
    ADD COLUMN IF NOT EXISTS minted_by_email TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS break_glass     BOOLEAN NOT NULL DEFAULT false,
    DROP CONSTRAINT IF EXISTS runner_registration_tokens_owner_kind,
    ADD CONSTRAINT runner_registration_tokens_owner_kind CHECK (owner_kind IN ('person', 'org')),
    DROP CONSTRAINT IF EXISTS runner_registration_tokens_org_owner_empty,
    ADD CONSTRAINT runner_registration_tokens_org_owner_empty CHECK (owner_kind = 'person' OR owner = ''),
    DROP CONSTRAINT IF EXISTS runner_registration_tokens_minted_by_kind,
    ADD CONSTRAINT runner_registration_tokens_minted_by_kind CHECK (minted_by_kind IN ('human', 'admin_token')),
    DROP CONSTRAINT IF EXISTS runner_registration_tokens_break_glass,
    ADD CONSTRAINT runner_registration_tokens_break_glass CHECK (NOT break_glass OR minted_by_kind = 'admin_token');
