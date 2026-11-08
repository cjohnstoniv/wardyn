-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Platform envelopes. A governance_envelopes row names one unit, the root of a platform, and the IdP
-- groups the security tier approved for it. It changes who must approve a write below the root and
-- nothing at run time. A separate table, so governance_units and its hash do not change.
--
-- A split-role install grants the app role SELECT, INSERT, UPDATE, DELETE on governance_envelopes
-- (docs/OPERATIONS.md).
--
-- Re-runnable: every statement is IF NOT EXISTS.
CREATE TABLE IF NOT EXISTS governance_envelopes (
    unit_id         UUID PRIMARY KEY REFERENCES governance_units (id) ON DELETE CASCADE,
    approved_groups TEXT[] NOT NULL DEFAULT '{}',
    set_by          TEXT NOT NULL,
    set_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
