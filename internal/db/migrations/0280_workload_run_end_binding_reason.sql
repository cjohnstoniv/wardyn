-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- A third reason a workload's run is owed its end: the namespace binding a custom resource's launch was
-- authorised under was deleted while the launch's run was starting, so the workload never took that run.
-- 0244 named the column's CHECK inline, so Postgres named it workload_run_ends_reason_check.
ALTER TABLE workload_run_ends DROP CONSTRAINT IF EXISTS workload_run_ends_reason_check;
ALTER TABLE workload_run_ends ADD CONSTRAINT workload_run_ends_reason_check
    CHECK (reason IN ('workload_replaced', 'workload_deleted', 'workload_binding_deleted'));
