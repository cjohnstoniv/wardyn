/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { canDecideApproval, type ApprovalKind } from "./approvals";

// canDecideApproval is the ONE predicate both approvals.tsx and run-detail.tsx
// gate their decide buttons on — it must mirror internal/api/approvals.go's
// decide() exactly: an admin/operator may decide anything; a member may
// decide only egress_domain (credential and tool_call stay admin-only
// REGARDLESS of ownership — self-approving a credential mint or re-opening a
// clamped tool_call would self-authorize under the operator's own ceiling).
describe("canDecideApproval", () => {
  it("an operator/admin may decide every kind", () => {
    for (const kind of ["credential", "egress_domain", "tool_call"] as ApprovalKind[]) {
      expect(canDecideApproval(true, kind)).toBe(true);
    }
  });

  it("a member may decide egress_domain only", () => {
    expect(canDecideApproval(false, "egress_domain")).toBe(true);
    expect(canDecideApproval(false, "credential")).toBe(false);
    expect(canDecideApproval(false, "tool_call")).toBe(false);
  });
});
