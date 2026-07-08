-- Record Mode: per-task recording results for the workspace import pipeline
-- (Source → Scan → Configure → Record → Verify → Finalize).
--
-- One opaque JSONB map (taskKey → result: run pointer, mode, status,
-- observations captured from the run's audit events, caveats). Rides the
-- existing opaque-blob precedent (profile/verify_result) — no new tables, no
-- status CHECK change: record is per-task and skippable, so the workspace
-- stays `scanned` throughout and serial concurrency rides active_run_id.
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS record_results JSONB;
