-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The user view looks through a chosen user type (0.8, user-types design
-- section 2.7).
--
-- principal_prefs holds per-person preferences that follow a person across
-- devices, keyed by the OIDC sub. The first key is user_view.type: the type
-- an admin last viewed as, which the view switch preselects. A preference is
-- a default, never a control: nothing reads it to decide what anyone may do.
CREATE TABLE IF NOT EXISTS principal_prefs (
    principal  TEXT NOT NULL,
    key        TEXT NOT NULL,
    value      JSONB NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (principal, key)
);

-- agent_runs.user_type freezes the user type a run's creator resolved as at
-- create time: the chosen type for a run launched in the user view, the
-- stamped one otherwise, '' for a run with no human creator. No FK: a type
-- may be deleted after its runs, which keep saying what they ran as.
-- NOT NULL DEFAULT '' as 0065_agent_runs_autonomy_level did.
ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS user_type TEXT NOT NULL DEFAULT '';
