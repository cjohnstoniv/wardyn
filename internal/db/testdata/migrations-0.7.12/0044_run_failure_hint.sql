-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- A run that dies BEFORE its agent ever starts (an image that resolves
-- control-plane-side but not on the daemon, a lost sandbox ref, an opaque-LLM
-- inspection refusal) used to land as a reason-less FAILED badge: the real
-- reason lived only in a run.create/run.dispatch audit row, never on the run
-- itself. failure_hint carries that one-line operator reason on the run row so
-- the cockpit can render WHY under the FAILED badge (D9). Stamped by
-- failAndRevoke; empty for every other run (a clean FAILED-by-nonzero-exit
-- carries its exit code instead).
--
-- NOT NULL DEFAULT '' — a metadata-only add on PG11+ — so scanRun keeps
-- scanning into a plain string and every legacy row reads "no hint" rather than
-- threading a nullable through the six agent_runs column lists.
ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS failure_hint TEXT NOT NULL DEFAULT '';
