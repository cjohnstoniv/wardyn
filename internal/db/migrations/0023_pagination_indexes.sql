-- Copyright 2025 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- Index the remaining list-sort / filter access paths that gained LIMIT/OFFSET
-- pagination (store.*Page methods, api.parseListPage). 0020 already covered the
-- two 5s-polled feeds — agent_runs(created_at DESC) and approvals(requested_at
-- DESC). This finishes the set for the endpoints the SDK/CLI/UI can now page.
--
-- Each index is justified against the exact query it serves (as 0020 does); only
-- the columns the queries ORDER BY / filter on are indexed, nothing speculative.

-- store.QueryAuditEventsPage: `WHERE run_id=$1 ORDER BY seq ASC [LIMIT/OFFSET]`.
-- 0001 indexed audit_events(run_id) alone, so the filter used the index but the
-- seq ordering was a separate sort of ALL of a busy run's rows before the LIMIT.
-- The composite (run_id, seq) resolves both: an index range scan on run_id that
-- is already seq-ordered, so LIMIT/OFFSET pages a run's trail with no sort and no
-- temp files (U071's run-scoped-sort residual; U048's paging past the per-run
-- cap). seq is BIGINT IDENTITY (globally monotonic), so ASC within a run_id is
-- exactly insertion order — the chronological trail docs/sdk.md documents.
CREATE INDEX IF NOT EXISTS audit_events_run_seq_idx ON audit_events (run_id, seq);

-- store.ListPoliciesPage / ListWorkspacesPage: `ORDER BY created_at DESC
-- [LIMIT/OFFSET]`. 0001 (run_policies) and 0008 (workspaces) indexed neither
-- table's created_at, so each list did a full seq scan + sort. These match the
-- ORDER BY direction so the planner takes a backward index scan (rows are
-- appended in ascending created_at, correlation≈1), same win 0020 measured for
-- agent_runs. Both tables are small today; the index keeps the sort off the hot
-- path as they grow and makes the new LIMIT a cheap top-N.
CREATE INDEX IF NOT EXISTS run_policies_created_at_idx ON run_policies (created_at DESC);
CREATE INDEX IF NOT EXISTS workspaces_created_at_idx ON workspaces (created_at DESC);

-- store.ListApprovalsPage single-state branch: `WHERE state=$1 ORDER BY
-- requested_at DESC [LIMIT/OFFSET]`. 0001's approvals_state_idx is PARTIAL
-- (WHERE state='PENDING') — it serves only the pending queue, not a filter on
-- APPROVED/DENIED/EXPIRED, and it carries no requested_at to satisfy the sort.
-- 0020's approvals_requested_at_idx serves the ALL-states feed but not a state
-- filter. This composite (state, requested_at DESC) serves any single-state
-- filter with an index range scan that is already in requested_at order — no
-- sort, no reliance on the partial index. It intentionally overlaps the partial
-- PENDING index for state='PENDING'; the partial stays because it is smaller and
-- the hot approve/deny path uses it.
CREATE INDEX IF NOT EXISTS approvals_state_requested_at_idx ON approvals (state, requested_at DESC);
