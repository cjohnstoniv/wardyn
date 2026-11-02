-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- image_arches are the architectures a run's prewrapped image was checked on, written once when the
-- run's image resolves to a ready prewrapped row (design final §3). Dispatch places the run by these,
-- not by the catalog, so a row deleted or re-added after the run's create changes nothing for it.
-- NULL for every other run. agent_runs already has its grants.
ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS image_arches TEXT[];
