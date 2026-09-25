-- Workspace Import v2: turn the one-shot scan→ready into a multi-stage,
-- resumable, iterative import (scan → configure → build → verify → finalize).
--
-- Widen the status lifecycle to model the pipeline stages, and add the
-- operator-owned + verify-result columns. Everything rides the existing
-- opaque-blob precedent (profile/approved_egress) — no new tables; per-step
-- history lives in the append-only audit_events log (workspace.import.* actions).

-- Widen the status CHECK to the full import lifecycle. The inline CHECK from
-- 0008 is auto-named workspaces_status_check; drop-if-exists then re-add.
ALTER TABLE workspaces DROP CONSTRAINT IF EXISTS workspaces_status_check;
ALTER TABLE workspaces ADD CONSTRAINT workspaces_status_check
    CHECK (status IN (
        'pending_scan','scanning','scanned','building','build_error',
        'verifying','verify_failed','ready','error'
    ));

-- Operator-approved setup commands (promoted from the scanner's advisory
-- profile.setup_commands; never auto-run). Opaque JSON, like approved_egress.
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS setup_commands JSONB;

-- Last verify run's per-step result (exit codes + bounded head+tail logs),
-- re-derived control-plane-side from the run's upload. Opaque, like profile.
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS verify_result JSONB;

-- "Proven-working" markers, distinct from built_profile_hash ("built").
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS verified_profile_hash TEXT NOT NULL DEFAULT '';
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS verified_at TIMESTAMPTZ;

-- The in-flight scan/build/verify run, so the panel can poll one step.
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS active_run_id UUID;
