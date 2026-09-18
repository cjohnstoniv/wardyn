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

import { absoluteTime } from "./format";
import type { SetupModelAccess, SetupStatus } from "./types";
import { AGENTS, MODEL_ACCESS_ACTIONABLE, isPerUserSsoRow } from "./workspace-providers-copy";

// The agent whose model-access lane the server grades. Mirrors
// internal/api/modelaccess.go's modelAccessAgent: bedrock_sso is claude-code's
// lane alone, so there is exactly one row to read.
export const MODEL_ACCESS_AGENT = "claude-code";

export interface ModelAccessDoor {
  /** The server's state name; "" when it graded nothing (a legacy install, an
   *  older daemon, or a status not fetched yet). */
  state: string;
  /** The server's own sentence, verbatim; "" when there is none. Never
   *  reworded client-side — see modelAccessActionLine for the ONE exception
   *  and why it is one. */
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

/**
 * modelAccessActionLine is the server's action line as a HUMAN's clock renders
 * it — the one place the console re-composes a server sentence, and only this
 * one: `expiring`'s action carries an RFC3339 UTC stamp ("Sign in again before
 * 2026-09-19T14:03:22Z"), which a member in another timezone misreads on every
 * screen for 24 hours. The template it is re-composed from is the SAME frozen
 * canon string the server formats (workspace-providers-prompt.md §7.7), so the
 * sentence is unchanged — only the instant is localised.
 *
 * Every other state, and an `expiring` from a daemon that sends no `deadline`,
 * renders verbatim.
 */
export function modelAccessActionLine(access: SetupModelAccess | undefined | null): string {
  if (!access?.action) return "";
  if (access.state === "expiring" && access.deadline) {
    return AGENTS.MODEL_ACCESS_EXPIRING_ACTION(absoluteTime(access.deadline));
  }
  return access.action;
}
