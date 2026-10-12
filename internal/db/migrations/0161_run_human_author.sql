-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The principal accountable for a run when it was created, written once in the run's
-- own INSERT and never re-derived: the service identity's sponsor at launch for a
-- service identity's run, and the creator for any other run. A later sponsor change
-- does not move it. It records accountability, not that a person was present. The
-- second-human rules read it beside the creator and the service's current sponsor;
-- a service identity's run with no value (one created before this column) is
-- treated as having no verifiable author and is refused there.
--
-- Split-role installs grant the app role no extra privilege here: this is a column
-- on a table it already writes.
ALTER TABLE agent_runs
    ADD COLUMN IF NOT EXISTS human_author TEXT NOT NULL DEFAULT '';
