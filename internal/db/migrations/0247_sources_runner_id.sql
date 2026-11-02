-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- A member's local_dir folder can live on one of their own runners: runner_id names it, and a run with
-- that folder is placed only there. The runner is part of the source's identity, so one path held on
-- the control plane's host and on a runner is two rows.
--
-- The key is NO ACTION, never SET NULL (a laptop path would silently become a path on the control
-- plane's host) and never CASCADE (the member's source would vanish). Revoking a runner never deletes
-- its row, and the unclaimed sweep deletes only rows never claimed, which no source can name.
--
-- The old identity, 0031's unnamed UNIQUE (kind, locator, ref), is dropped BEFORE the new index, or two
-- rows at one path would collide on it. NULL runner_id is folded to the nil uuid in the index, so an
-- unbound path stays unique without NULLS NOT DISTINCT (Postgres 15+; the floor is 13).
--
-- FORWARD-ONLY. A downgrade with a bound row present is unsupported: an older server reads the folder
-- as one on its own host.
ALTER TABLE sources ADD COLUMN IF NOT EXISTS runner_id UUID REFERENCES runners(id);
ALTER TABLE sources DROP CONSTRAINT IF EXISTS sources_kind_locator_ref_key;
CREATE UNIQUE INDEX IF NOT EXISTS sources_identity_idx
    ON sources (kind, locator, ref, (COALESCE(runner_id, '00000000-0000-0000-0000-000000000000'::uuid)));
