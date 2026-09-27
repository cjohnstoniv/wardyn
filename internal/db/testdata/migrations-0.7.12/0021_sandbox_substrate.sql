-- Durable sandbox ref -> substrate routing for the orchestrator.
--
-- The orchestrator tracks which substrate created each sandbox in an in-memory
-- map (byRef) so lifecycle ops route back to the right substrate. A
-- control-plane restart empties that map: with ONE substrate subForRef falls
-- back to it and crash recovery still works, but a MULTI-substrate deployment
-- then answers "no substrate tracked for ref" for every pre-restart run —
-- Exec/Wait/Attach/Status/Stop/Kill all fail, i.e. the KILL SWITCH is dead
-- exactly in the deployment shape the substrate seam exists to enable.
--
-- Persist the mapping by substrate NAME (a substrate object is not
-- serialisable; names are stable wiring identifiers) so a fresh orchestrator
-- rehydrates the route on the first byRef miss. Rows are written through
-- synchronously at CreateSandbox (fail-closed: a sandbox whose row can't be
-- written is torn down, never handed out) and deleted best-effort on
-- successful stop/kill so the table doesn't grow without bound.
CREATE TABLE IF NOT EXISTS sandbox_substrates (
	ref            TEXT PRIMARY KEY,
	substrate_name TEXT NOT NULL,
	created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
