-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The SHA-256 of a run's effective policy (types.RunPolicySpec.Digest), written once at dispatch, just
-- before the run.policy.resolve row that records the policy. Every later audit row of the run carries it
-- as data.policy_digest. '' is a run dispatched before this column, or one whose write failed, which is
-- every existing row. agent_runs already has its grants.
ALTER TABLE agent_runs
    ADD COLUMN IF NOT EXISTS policy_digest TEXT NOT NULL DEFAULT '';
