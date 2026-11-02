-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- A service is a background command that is kept running: its spec (restart, port, health check) is
-- frozen on the run at create, and the last health wardynd observed is mirrored beside it. Both are
-- NULL on every other run. service is written at insert; service_status only by its own scoped
-- UPDATE, which never moves updated_at (the idle and tail-upload clock). agent_runs already has its
-- grants.
ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS service JSONB;
ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS service_status JSONB;
