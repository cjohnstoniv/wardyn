-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Give a run a NAME. Until now a run was identified by its task text, which an
-- interactive run does not have at all — so half the console's run rows read
-- "—" and there was no way to say "these fourteen runs are the same piece of
-- work". Runs that share a title are grouped in the console's run list.
--
-- NOT NULL DEFAULT '' rather than nullable: a metadata-only add on PG11+, and
-- it keeps scanRun scanning into a plain string instead of threading
-- *string/sql.NullString through six column lists. Empty for every legacy row
-- and for the system runs (scan, harness login, workspace record/verify) that
-- have no human to name them; the console falls back to task for display.
--
-- ponytail: no title index — grouping is client-side over the already-fetched
-- run list. Add one if a server-side GROUP BY ever ships.
ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS title       TEXT NOT NULL DEFAULT '';
ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS description TEXT NOT NULL DEFAULT '';
