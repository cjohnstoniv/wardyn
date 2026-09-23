/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import {
  ADO_HOLD_WINDOW_MS,
  canDecideApproval,
  decisionArgs,
  isHeld,
  isStaleHold,
  type ApprovalKind,
  type ApprovalRequest,
} from "./approvals";

const approval = (over: Partial<ApprovalRequest> = {}): ApprovalRequest => ({
  id: "a1",
  run_id: "run-1",
  kind: "tool_call",
  requested_scope: {},
  state: "PENDING",
  requested_at: new Date().toISOString(),
  ...over,
});

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

// decisionArgs feeds approve/deny's trailing (optional) argument — pinned
// directly because getting this wrong is silent: an unconditional third arg
// doesn't throw, it just makes every "run"-scope decide call 3-argument
// instead of 2-, which several vitest suites assert exactly against.
describe("decisionArgs", () => {
  it("the default 'run' scope produces NO trailing arg — a literal 2-arg call", () => {
    expect(decisionArgs("run")).toEqual([]);
    expect(decisionArgs("run", undefined)).toEqual([]);
  });

  it("every non-default scope produces exactly one options object", () => {
    expect(decisionArgs("once")).toEqual([{ scope: "once", until: undefined }]);
    expect(decisionArgs("always")).toEqual([{ scope: "always", until: undefined }]);
    expect(decisionArgs("until", "2030-01-01T00:00:00.000Z")).toEqual([
      { scope: "until", until: "2030-01-01T00:00:00.000Z" },
    ]);
  });
});

describe("canDecideApproval — the re-auth kind", () => {
  it("is decidable by NOBODY, security operator included", () => {
    // It is resolved by its owner signing in, and the server answers 409 to an
    // approve or a deny (decide()'s rule 3b). A Deny would read as an act of
    // governance and change nothing: findPendingDup matches PENDING only, so
    // the sidecar's very next resolve would raise a fresh row.
    expect(canDecideApproval(true, "credential_reauth")).toBe(false);
    expect(canDecideApproval(false, "credential_reauth")).toBe(false);
  });

  it("leaves every other kind exactly as it was", () => {
    expect(canDecideApproval(false, "egress_domain")).toBe(true);
    expect(canDecideApproval(false, "credential")).toBe(false);
    expect(canDecideApproval(true, "credential")).toBe(true);
    expect(canDecideApproval(true, "tool_call")).toBe(true);
  });
});

// #160 — isHeld's stale-hold ceiling on its two unconditional arms (tool_call,
// credential_reauth): 60 minutes, NOT the 30s HOLD_TIMEOUT_MS the egress
// wait_for_review arm below uses — a different arm entirely, left untouched.
// Both isHeld's callers (the runs board via board-groups.ts, and the run
// cockpit's command bar via run-detail.tsx's `pending.some(isHeld)`) share
// this one predicate, so pinning it here pins both call sites at once.
describe("isHeld / isStaleHold — the 60-minute ceiling on tool_call/credential_reauth", () => {
  it("a fresh tool_call is held; a fresh credential_reauth is held", () => {
    expect(isHeld(approval({ kind: "tool_call" }))).toBe(true);
    expect(isHeld(approval({ kind: "credential_reauth" }))).toBe(true);
    expect(isStaleHold(approval({ kind: "tool_call" }))).toBe(false);
    expect(isStaleHold(approval({ kind: "credential_reauth" }))).toBe(false);
  });

  it("a tool_call/credential_reauth row past 60 minutes is no longer held, and IS stale", () => {
    const old = new Date(Date.now() - 61 * 60_000).toISOString();
    for (const kind of ["tool_call", "credential_reauth"] as const) {
      expect(isHeld(approval({ kind, requested_at: old }))).toBe(false);
      expect(isStaleHold(approval({ kind, requested_at: old }))).toBe(true);
    }
  });

  it("a row well short of 60 minutes (past isHeld's unrelated 30s egress ceiling) is still held, not stale", () => {
    const fiveMinutesAgo = new Date(Date.now() - 5 * 60_000).toISOString();
    for (const kind of ["tool_call", "credential_reauth"] as const) {
      expect(isHeld(approval({ kind, requested_at: fiveMinutesAgo }))).toBe(true);
      expect(isStaleHold(approval({ kind, requested_at: fiveMinutesAgo }))).toBe(false);
    }
  });

  it("an unparseable requested_at fails TOWARD showing the hold, not toward stale", () => {
    expect(isHeld(approval({ kind: "tool_call", requested_at: "not-a-date" }))).toBe(true);
    expect(isStaleHold(approval({ kind: "tool_call", requested_at: "not-a-date" }))).toBe(false);
  });

  it("isStaleHold is false for every other kind, at any age — this arm is tool_call/credential_reauth only", () => {
    const old = new Date(Date.now() - 61 * 60_000).toISOString();
    expect(isStaleHold(approval({ kind: "egress_domain", requested_scope: { host: "h" }, requested_at: old }))).toBe(false);
    expect(isStaleHold(approval({ kind: "credential", requested_at: old }))).toBe(false);
  });

  it("egress wait_for_review keeps its own 30s ceiling, unaffected by the new 60-minute one", () => {
    const past30s = new Date(Date.now() - 60_000).toISOString();
    const held = approval({ kind: "egress_domain", requested_scope: { host: "h", mode: "wait_for_review" }, requested_at: past30s });
    expect(isHeld(held)).toBe(false); // still the pre-existing 30s behaviour
    expect(isStaleHold(held)).toBe(false); // not this arm at all — falls to passiveHold upstream
  });
});

// #725/F1 — an Azure DevOps capability escalation IS a tool_call row
// (isAdoCapabilityRequest: grant_id set, requested_scope.lane
// "azure_devops"), but the proxy releases its hold after ADO_HOLD_WINDOW_MS
// (4 minutes), not the generic 60-minute ceiling above. Before this arm,
// isHeld kept reporting "held" for up to an hour while the ADO card itself
// (ado-capability-card.tsx's own stillHeld, the same 4-minute window) had
// already flipped to "no longer waiting" — the board and the card disagreed.
const adoApproval = (over: Partial<ApprovalRequest> = {}): ApprovalRequest =>
  approval({
    kind: "tool_call",
    grant_id: "grant-1",
    requested_scope: { lane: "azure_devops", provider_id: "p1", org: "o1", grant_id: "grant-1", capability: "pr", repo: "r1", tool: "t", cmd: "c" },
    ...over,
  });

describe("isHeld — an Azure DevOps capability escalation's own 4-minute window (#725/F1)", () => {
  it("a fresh ADO tool_call is held", () => {
    expect(isHeld(adoApproval())).toBe(true);
  });

  it("an ADO tool_call row at 5 minutes is no longer held, well inside the generic 60-minute ceiling", () => {
    const fiveMinutesAgo = new Date(Date.now() - 5 * 60_000).toISOString();
    expect(isHeld(adoApproval({ requested_at: fiveMinutesAgo }))).toBe(false);
    // Not yet the generic stale-hold ceiling either — isStaleHold uses the
    // unconditional 60-minute window, unchanged by this fix (board-groups.ts
    // reads it as a passive pending, not a stale one, at this age).
    expect(isStaleHold(adoApproval({ requested_at: fiveMinutesAgo }))).toBe(false);
  });

  it("right at ADO_HOLD_WINDOW_MS, isHeld matches the exported constant, not a hand-copied number", () => {
    const justUnder = new Date(Date.now() - (ADO_HOLD_WINDOW_MS - 1000)).toISOString();
    const justOver = new Date(Date.now() - (ADO_HOLD_WINDOW_MS + 1000)).toISOString();
    expect(isHeld(adoApproval({ requested_at: justUnder }))).toBe(true);
    expect(isHeld(adoApproval({ requested_at: justOver }))).toBe(false);
  });

  it("a plain tool_call with no grant_id/azure_devops scope keeps the 60-minute ceiling at 5 minutes", () => {
    const fiveMinutesAgo = new Date(Date.now() - 5 * 60_000).toISOString();
    expect(isHeld(approval({ kind: "tool_call", requested_at: fiveMinutesAgo }))).toBe(true);
  });
});
