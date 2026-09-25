/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Test-only factories for the wire types tests fabricate most often. Each
// returns a complete, type-checked default object spread with the caller's
// overrides — no `as` cast, so a field added/renamed on the wire type fails
// `tsc` here instead of drifting silently past a cast. See #195.
import { aheadByHours } from "../app/lib/test-clock";
import type { AgentRun } from "../app/lib/types/runs";
import type { Workspace } from "../app/lib/types/workspaces";
import type { ApprovalRequest } from "../app/lib/types/approvals";

export function makeRun(o: Partial<AgentRun> = {}): AgentRun {
  return {
    id: "run-1",
    created_at: aheadByHours(-1),
    updated_at: aheadByHours(-1),
    created_by: "test-user",
    agent: "claude-code",
    repo: "acme/repo",
    task: "test task",
    confinement_class: "CC1",
    state: "RUNNING",
    spiffe_id: "spiffe://wardyn/run-1",
    runner_target: "local",
    ...o,
  };
}

export function makeWorkspace(o: Partial<Workspace> = {}): Workspace {
  return {
    id: "ws-1",
    name: "test-workspace",
    kind: "repo",
    source: "acme/repo",
    status: "scanned",
    created_at: aheadByHours(-1),
    updated_at: aheadByHours(-1),
    ...o,
  };
}

export function makeApproval(o: Partial<ApprovalRequest> = {}): ApprovalRequest {
  return {
    id: "approval-1",
    run_id: "run-1",
    kind: "tool_call",
    requested_scope: {},
    state: "PENDING",
    requested_at: aheadByHours(-1),
    ...o,
  };
}
