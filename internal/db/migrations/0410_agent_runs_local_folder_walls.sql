-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- A run's walls for folders from a person's computer (#2990), as dispatch resolved them, with the layer that set
-- them. The folder doors read them here, never from the run's policy audit rows, which audit retention can prune.
-- NULL is no walls: every run from before this column was created before the walls existed. Additive only.
ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS local_folder_walls JSONB;
