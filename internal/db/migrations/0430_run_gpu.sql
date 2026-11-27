-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- A GPU for a container run (#2958). agent_runs.gpu records the GPU a run was given: '' (none, every run before this file),
-- 'nvidia' or 'render'. ADDITIVE ONLY: a constant-default column, so no row is rewritten. A downgrade stops reading it.
ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS gpu TEXT NOT NULL DEFAULT '';
ALTER TABLE agent_runs
    DROP CONSTRAINT IF EXISTS agent_runs_gpu,
    ADD CONSTRAINT agent_runs_gpu CHECK (gpu IN ('', 'nvidia', 'render'));

-- Asking for a GPU needs the `feature` capability value `gpu`. This row restricts that value: only an allow grant that names
-- it admits anyone, so a `*` allow grants nobody, as 0239 does for `inbound`. ON CONFLICT keeps an admin's own choice.
INSERT INTO capability_restrictions (capability, value, created_by)
VALUES ('feature', 'gpu', 'migration:0430')
ON CONFLICT (capability, value) DO NOTHING;
