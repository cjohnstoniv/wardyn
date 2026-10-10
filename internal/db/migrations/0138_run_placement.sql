-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Where a run's sandbox lives (0.9 client mode, docs/design/0.9/PLAN.md §6.1):
-- the placement the run landed on, whether that placement was resolved rather
-- than asked for, the runner it ran on, and who can attest its evidence.
--
-- Written ONCE at create, by the placement lane (#117) that resolves it — the
-- same "recorded, never re-derived" rule 0117's sizing columns follow: a later
-- change to the deployment's eligible placements must not move a recorded run.
--
-- placement '' is a run from before 0.9 and any run whose placement nothing has
-- recorded; placement_filled false means the value was not resolved by the
-- resolver, so a review can tell "the person chose local" from "local was the
-- only eligible placement".
--
-- runner_id is NULL for a remote run and for every pre-0.9 run. It is a foreign
-- key to runners because a run on a runner that no longer exists cannot be
-- dispatched, revived or reconciled; ON DELETE SET NULL rather than CASCADE,
-- because erasing a person's runner must not erase the runs that ran on it (the
-- run row is the audit record, and its placement stays readable).
--
-- evidence_source is NOT confinement_source, which says who CHOSE the
-- confinement class ('requested' or 'defaulted') and lives on the audit row.
-- This says who can ATTEST the run's evidence: the organisation's own substrate,
-- or a runner that said so and nobody can verify ('runner_asserted'). Empty only
-- for a pre-0.9 record.
--
-- Split-role installs grant the app role no extra privilege here: these are
-- columns on a table it already writes.
ALTER TABLE agent_runs
    ADD COLUMN IF NOT EXISTS placement         TEXT NOT NULL DEFAULT '' CHECK (placement IN ('', 'local', 'remote')),
    ADD COLUMN IF NOT EXISTS placement_filled  BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS runner_id         UUID REFERENCES runners(id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS evidence_source   TEXT NOT NULL DEFAULT '' CHECK (evidence_source IN ('', 'substrate', 'runner_asserted'));

-- The capacity and eligibility reads filter on placement, and the runner's own
-- run list filters on runner_id.
CREATE INDEX IF NOT EXISTS agent_runs_placement_idx ON agent_runs (placement) WHERE placement <> '';
CREATE INDEX IF NOT EXISTS agent_runs_runner_id_idx ON agent_runs (runner_id) WHERE runner_id IS NOT NULL;