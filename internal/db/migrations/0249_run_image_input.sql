-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The image ref a run's create held to the digest rule (images.require_digest), written once at insert:
-- the ref the member named or an attached workspace's base image set, '' when the run named none (the
-- operator's convention image, and every run Wardyn launches itself). A revive re-runs create's decision
-- on it under the rule as it stands then. NULL is a run created before this column, which the rule, while
-- on, refuses at revive until a new run starts. agent_runs already has its grants.
ALTER TABLE agent_runs
    ADD COLUMN IF NOT EXISTS image_input TEXT;
