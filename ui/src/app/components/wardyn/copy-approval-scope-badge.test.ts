/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// approvalScopeBadge's Azure DevOps extension (S10 round 2, F11). Its own
// file rather than a new describe in copy.ts's existing suite (none exists
// yet for this function) — a pure-function test, cheap and precise.

import { describe, it, expect } from "vitest";
import { approvalScopeBadge } from "./copy";
import { ADO_CAPABILITY } from "../../lib/ado-capability-copy";

const adoItem = (over: Record<string, unknown> = {}) => ({
  kind: "tool_call" as const,
  state: "APPROVED" as const,
  decision_scope: "once" as const,
  decision_expires_at: undefined,
  grant_id: "grant_1",
  requested_scope: { lane: "azure_devops", capability: "code_write" },
  ...over,
});

describe("approvalScopeBadge — the Azure DevOps escalation badge", () => {
  it("reads 'Allowed once' for an approved once-scoped escalation", () => {
    expect(approvalScopeBadge(adoItem({ decision_scope: "once" }))).toBe(ADO_CAPABILITY.OUTCOME_ALLOWED_ONCE);
  });

  it("reads 'Allowed for this run' for an approved run-scoped escalation", () => {
    expect(approvalScopeBadge(adoItem({ decision_scope: "run" }))).toBe(ADO_CAPABILITY.OUTCOME_ALLOWED_RUN);
  });

  it("carries no badge for a denied escalation — the state chip already says Denied", () => {
    expect(approvalScopeBadge(adoItem({ state: "DENIED" }))).toBeUndefined();
  });

  it("carries no badge for a still-PENDING escalation", () => {
    expect(approvalScopeBadge(adoItem({ state: "PENDING", decision_scope: undefined }))).toBeUndefined();
  });

  it("an ordinary (non-Azure-DevOps) tool_call gets no badge, same as before", () => {
    expect(
      approvalScopeBadge({
        kind: "tool_call",
        state: "APPROVED",
        decision_scope: "run",
        grant_id: undefined,
        requested_scope: { tool: "bash" },
      }),
    ).toBeUndefined();
  });

  it("egress_domain badges are unaffected — still the lowercase scope word", () => {
    expect(
      approvalScopeBadge({
        kind: "egress_domain",
        state: "APPROVED",
        decision_scope: "run",
        requested_scope: { host: "api.example.com" },
      }),
    ).toBe("this run");
  });
});
