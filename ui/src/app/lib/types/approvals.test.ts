/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { canDecideApproval, decisionArgs, isHeld, isStaleHold, type ApprovalKind, type ApprovalRequest } from "./approvals";

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

// #509 — isHeld's two unconditional arms (tool_call, credential_reauth) no
// longer time out client-side at all: PENDING is live until the SERVER moves
// the row on, because the sandbox stays parked on a PENDING row for up to
// WARDYN_APPROVAL_EXPIRY_AFTER (24h default), not the 60-minute ceiling #160
// invented. Both isHeld's callers (the runs board via board-groups.ts, and the
// run cockpit's command bar via run-detail.tsx's `pending.some(isHeld)`) share
// this one predicate, so pinning it here pins both call sites at once.
describe("isHeld / isStaleHold — a hold stays live until the server's own state says otherwise", () => {
  it("a fresh tool_call is held; a fresh credential_reauth is held", () => {
    expect(isHeld(approval({ kind: "tool_call" }))).toBe(true);
    expect(isHeld(approval({ kind: "credential_reauth" }))).toBe(true);
    expect(isStaleHold(approval({ kind: "tool_call" }))).toBe(false);
    expect(isStaleHold(approval({ kind: "credential_reauth" }))).toBe(false);
  });

  it("a PENDING tool_call/credential_reauth row 2 hours old is still held, not stale — past the old 60-minute ceiling but well inside the server's 24h expiry", () => {
    const twoHoursAgo = new Date(Date.now() - 2 * 60 * 60_000).toISOString();
    for (const kind of ["tool_call", "credential_reauth"] as const) {
      expect(isHeld(approval({ kind, requested_at: twoHoursAgo }))).toBe(true);
      expect(isStaleHold(approval({ kind, requested_at: twoHoursAgo }))).toBe(false);
    }
  });

  it("a PENDING tool_call/credential_reauth row 25 hours old — past the server's own 24h default — is STILL held: only the server's own state, never client elapsed time, ends the hold", () => {
    const twentyFiveHoursAgo = new Date(Date.now() - 25 * 60 * 60_000).toISOString();
    for (const kind of ["tool_call", "credential_reauth"] as const) {
      expect(isHeld(approval({ kind, requested_at: twentyFiveHoursAgo }))).toBe(true);
      expect(isStaleHold(approval({ kind, requested_at: twentyFiveHoursAgo }))).toBe(false);
    }
  });

  it("once the server has actually moved a tool_call/credential_reauth row to EXPIRED, it reads not held and IS stale — regardless of age", () => {
    const fresh = new Date().toISOString();
    for (const kind of ["tool_call", "credential_reauth"] as const) {
      const expired = approval({ kind, state: "EXPIRED", requested_at: fresh });
      expect(isHeld(expired)).toBe(false);
      expect(isStaleHold(expired)).toBe(true);
    }
  });

  it("a decided (APPROVED/DENIED/CANCELLED) tool_call/credential_reauth row is not held, and is not a stale hold either — it was resolved, not abandoned", () => {
    for (const kind of ["tool_call", "credential_reauth"] as const) {
      for (const state of ["APPROVED", "DENIED", "CANCELLED"] as const) {
        const decided = approval({ kind, state });
        expect(isHeld(decided)).toBe(false);
        expect(isStaleHold(decided)).toBe(false);
      }
    }
  });

  it("isStaleHold is false for every other kind, even EXPIRED — this arm is tool_call/credential_reauth only", () => {
    expect(isStaleHold(approval({ kind: "egress_domain", requested_scope: { host: "h" }, state: "EXPIRED" }))).toBe(false);
    expect(isStaleHold(approval({ kind: "credential", state: "EXPIRED" }))).toBe(false);
  });

  it("egress wait_for_review keeps its own 30s ceiling, unaffected by the tool_call/credential_reauth change", () => {
    const past30s = new Date(Date.now() - 60_000).toISOString();
    const held = approval({ kind: "egress_domain", requested_scope: { host: "h", mode: "wait_for_review" }, requested_at: past30s });
    expect(isHeld(held)).toBe(false); // still the pre-existing 30s behaviour
    expect(isStaleHold(held)).toBe(false); // not this arm at all — falls to passiveHold upstream
  });
});
