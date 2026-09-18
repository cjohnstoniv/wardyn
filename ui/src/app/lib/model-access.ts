/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// THE model-access predicate — one answer to "is there something about this
// person's model credential that they should be told about, and can they fix
// it?", read by every surface that offers the sign-in (the shell strip, the New
// Run rail, a credential-failed run's failure block, a held run's approval row).
//
// Not an extension of model-key-state.ts: that answers a CARD-shaped question
// (done / revealAllowed / band) keyed on modelKeyProvider's roster-ORDER row,
// which is the wrong row for this — model_access grades the claude-code row
// alone (internal/api/modelaccess.go's modelAccessAgent).
//
// Pure, no React: the context above it (components/wardyn/model-access-context)
// is what makes it reachable without prop-drilling through screens that are at
// the file-size gate.

// NOTHING from workspace-providers-copy, deliberately: this module is reached
// from the shell's EAGER graph (App.tsx -> the model-access context), and that
// module carries the whole AGENTS copy table — one import of it from here put
// 16 kB of copy into the entry chunk (bundle-split.test.ts). So the two POLICY
// exports below live here, beside the predicate that reads them, and
// workspace-providers-copy re-exports them for its existing callers. The one
// copy decision this feature needs of that table — `expiring`'s action line,
// re-composed on the reader's clock — lives THERE, beside its template.
import type { SetupHarnessTool, SetupStatus } from "./types";

// U-10: the per_user "something actionable to do" states — the member's own
// sign-in. Defined once so the Agents tab (admin), member Getting Started
// (member) and the shell strip share ONE policy instead of independently-typed
// literal sets that could drift on a sixth state.
export const MODEL_ACCESS_ACTIONABLE = new Set(["not_configured", "expired_signin", "expiring"]);

// R-01 (fix-console-u review): the "is this harness row a per-person AWS SSO
// lane" predicate — `h.enabled !== false` (not truthiness) is load-bearing, not
// decorative: a DISABLED row still legally carries mechanism/credential_source
// (validateAgentCredentialSource never looks at Disabled), but the server's
// login predicate (perUserLoginRow) and its model_access scoping
// (awsSSOScopeFor) both treat a disabled row as NOT per_user — grading it in
// the operator's own namespace and rejecting an empty start URL with a 400 a
// card would otherwise hide the field for. Absent `enabled` reads as unknown,
// never false, so an older daemon that omits the field is unaffected. ONE
// predicate, not two independently-typed copies (the U-03 recurrence this
// fixes).
export function isPerUserSsoRow(h: SetupHarnessTool): boolean {
  return h.enabled !== false && h.mechanism === "bedrock_sso" && h.credential_source === "per_user";
}

// The agent whose model-access lane the server grades. Mirrors
// internal/api/modelaccess.go's modelAccessAgent: bedrock_sso is claude-code's
// lane alone, so there is exactly one row to read.
export const MODEL_ACCESS_AGENT = "claude-code";

export interface ModelAccessDoor {
  /** The server's state name; "" when it graded nothing (a legacy install, an
   *  older daemon, or a status not fetched yet). */
  state: string;
  /** The server's own sentence, verbatim; "" when there is none. Never
   *  reworded client-side — see workspace-providers-copy.ts's modelAccessActionLine
   *  for the ONE exception, and why it is one. */
  action: string;
  /** The instant `action` names, RFC3339 UTC; "" when the state carries none or
   *  the daemon predates the field. Rendered through the viewer's own clock,
   *  never as the raw stamp. */
  deadline: string;
  /** Graded, and neither live nor not_applicable — the superset that INCLUDES
   *  shared_expired, which is text with no button. */
  needsAttention: boolean;
  /** A sign-in THIS caller can complete repairs it. */
  actionable: boolean;
  /** The claude-code row is an enabled per_user + bedrock_sso lane. */
  perUser: boolean;
}

const NO_DOOR: ModelAccessDoor = {
  state: "",
  action: "",
  deadline: "",
  needsAttention: false,
  actionable: false,
  perUser: false,
};

/** The door's fail-open default: nothing graded, nothing to say. Exported so a
 *  screen mounted with no provider above it (every suite that renders one
 *  directly) reads exactly today's output. */
export const NO_MODEL_ACCESS_DOOR = NO_DOOR;

/**
 * modelAccessDoor grades one /setup/status body for one viewer.
 *
 * `actionable` is AUDIENCE-AWARE for the shared-dead state, and that arm is
 * load-bearing: awsSSOCredentialState returns `shared_expired` for a missing OR
 * dead shared credential for its ADMIN too (setupModelAccess grades the
 * credential's SCOPE, not the viewer's role — only a pin contradiction forces
 * expired_signin), so without it an operator with an ordinary dead shared
 * credential reads "ask your admin" with no button. Their door is the pane with
 * startURLManaged=false, which authorizeHarnessLogin admits for ANY operator;
 * a member under that row keeps the admin instruction and no button.
 *
 * There is deliberately NO perUser gate on the per-user states: the server
 * admits an operator's sign-in unconditionally, and memberModelAccess already
 * guarantees a member under a shared row never sees an actionable state — so
 * gating on perUser would break the shared-row admin's working repair path.
 */
export function modelAccessDoor(
  status: SetupStatus | null | undefined,
  viewer: { operator: boolean },
): ModelAccessDoor {
  const access = status?.model_access;
  const state = access?.state ?? "";
  if (!state) return NO_DOOR;
  return {
    state,
    action: access?.action ?? "",
    deadline: access?.deadline ?? "",
    needsAttention: state !== "live" && state !== "not_applicable",
    actionable: MODEL_ACCESS_ACTIONABLE.has(state) || (state === "shared_expired" && viewer.operator),
    // The connection-cards.tsx composition, lifted rather than copied a third
    // time: ONE predicate decides whether this deployment gives each person
    // their own sign-in.
    perUser: !!status?.harnesses?.some((h) => h.id === MODEL_ACCESS_AGENT && isPerUserSsoRow(h)),
  };
}
