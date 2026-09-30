-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The console's "last token" line (#1428) reads a person's newest token on a
-- row from every ado_run_pats row, revoked ones included, on each
-- /me/scm-access and /setup/status read; 0102's index covers only the live
-- ones.
CREATE INDEX IF NOT EXISTS ado_run_pats_owner_row_created_idx
    ON ado_run_pats (owner, provider_row_id, created_at DESC);
