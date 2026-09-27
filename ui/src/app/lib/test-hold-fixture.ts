/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #1197 L1b moved isHeld's rule server-side (internal/approval.Hold); the
// wire now carries its answer as held/held_until rather than a client
// computing it from kind/requested_at/state. This is a TEST-ONLY mirror of
// that same rule (the three windows and the arm order are pinned server-side
// by internal/approval/hold_test.go) so every existing fixture that used to
// lean on isHeld's OWN client-side computation can keep constructing rows the
// same way — by kind/requested_at/state — and have held/held_until come out
// as the server would have sent them, instead of every call site hand-rolling
// its own held_until arithmetic.
import type { ApprovalKind, ApprovalRequest, ApprovalState } from "./types/approvals";

// The same three windows internal/types/holds.go names, duplicated here
// because production no longer exports them (the whole point of the move —
// see approvals.ts's isHeld doc). A drift between these and the server's own
// constants would show up as a hold_test.go boundary case this file's own
// callers don't reach, not as a silent client mismatch: the server, not this
// file, is now authoritative.
const ADO_WINDOW_MS = 240_000;
const PUSH_WINDOW_MS = 600_000;
const EGRESS_WINDOW_MS = 30_000;

function boundedHold(requestedAt: string, windowMs: number): { held: boolean; held_until: string } {
  const t = Date.parse(requestedAt);
  if (Number.isNaN(t)) return { held: true, held_until: new Date(Date.now() + windowMs).toISOString() };
  return { held: Date.now() - t < windowMs, held_until: new Date(t + windowMs).toISOString() };
}

/** The subset of ApprovalRequest heldFieldsFor needs to compute its answer. */
export type HoldFixtureInput = Pick<ApprovalRequest, "kind" | "state" | "requested_at" | "requested_scope"> & {
  grant_id?: string;
};

/**
 * heldFieldsFor mirrors internal/approval.Hold's arms, in the SAME order:
 * an Azure DevOps capability escalation (240s), any other tool_call or a
 * credential_reauth (unconditional while PENDING), push_content (600s), and
 * finally requested_scope.mode === "wait_for_review" (30s) — everything
 * else is a passive pending. Spread the result into a fixture:
 * `approval({ kind, requested_at, ...heldFieldsFor({ kind, requested_at, state, requested_scope }) })`.
 */
export function heldFieldsFor(a: HoldFixtureInput): { held?: boolean; held_until?: string } {
  if (a.state !== "PENDING") return {};
  const isADO = a.kind === "tool_call" && !!a.grant_id && a.requested_scope?.lane === "azure_devops";
  if (isADO) return boundedHold(a.requested_at, ADO_WINDOW_MS);
  if (a.kind === "tool_call" || a.kind === "credential_reauth") return { held: true };
  if (a.kind === "push_content") return boundedHold(a.requested_at, PUSH_WINDOW_MS);
  if (String((a.requested_scope?.mode as string) ?? "") !== "wait_for_review") return { held: false };
  return boundedHold(a.requested_at, EGRESS_WINDOW_MS);
}

// Re-exported so a fixture file that needs the raw window (e.g. to build a
// timestamp just past it) reads ONE number rather than a magic literal.
export const TEST_HOLD_WINDOWS = { ado: ADO_WINDOW_MS, push: PUSH_WINDOW_MS, egress: EGRESS_WINDOW_MS } as const;

// Referenced only for the Pick<> above; re-exported so a fixture file need
// not separately import these two purely for typing heldFieldsFor's callers.
export type { ApprovalKind, ApprovalState };
