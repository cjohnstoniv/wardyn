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
import type { SetupHarnessTool, SetupModelProvider, SetupStatus } from "./types";

// U-10: the per_user "something actionable to do" states — the member's own
// sign-in. Defined once so the Agents tab (admin), member Getting Started
// (member) and the shell strip share ONE policy instead of independently-typed
// literal sets that could drift on a sixth state.
export const MODEL_ACCESS_ACTIONABLE = new Set(["not_configured", "expired_signin", "expiring"]);

// R-01: the "is this harness row a per-person AWS SSO
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

// isPerUserBearerRow is isPerUserSsoRow's bedrock_bearer twin (#337): the row
// under which a MEMBER's own Bedrock bearer key is the credential their runs
// actually authenticate with (bedrockBearerFor, runs_bedrock.go), so the
// Settings card's bearer field may be theirs to edit. Same `enabled !== false`
// reasoning as isPerUserSsoRow — a disabled row is not per_user to the server
// either (validateAgentCredentialSource never looks at Disabled, but the
// injection sink's namespace resolve does).
export function isPerUserBearerRow(h: SetupHarnessTool): boolean {
  return h.enabled !== false && h.mechanism === "bedrock_bearer" && h.credential_source === "per_user";
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
  /** The claude-code row is an enabled bedrock_sso lane TODAY, per_user or
   *  shared — the weaker half of `perUser`, and the one a surface bound to a
   *  PAST event needs: a failed run's declared lane is history, and an AWS
   *  sign-in repairs nothing for an agent the roster has since moved to another
   *  mechanism (Codex #14). `model_access` keeps grading a captured session
   *  after such a move (setupModelAccess grades it whenever a blob is found),
   *  so `actionable` alone does not answer this. */
  bedrockSSO: boolean;
}

const NO_DOOR: ModelAccessDoor = {
  state: "",
  action: "",
  deadline: "",
  needsAttention: false,
  actionable: false,
  perUser: false,
  bedrockSSO: false,
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
 * admits an operator's sign-in unconditionally, and userModelAccess already
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
    // isPerUserSsoRow without its credential_source half: the shared row's
    // ADMIN has a working door too (authorizeHarnessLogin admits any operator),
    // and `enabled !== false` is load-bearing here for the same reason it is
    // there — the server treats a disabled row as no per-user lane at all.
    bedrockSSO: !!status?.harnesses?.some(
      (h) => h.id === MODEL_ACCESS_AGENT && h.enabled !== false && h.mechanism === "bedrock_sso",
    ),
  };
}

/** "Claude Code" / "Claude Code and Codex CLI" — the display names of the
 *  given harness ids, off the roster's own names. Shared by the strip (B1,
 *  B4, B5) and Your model connections' own "For …" line (§5.4): one join, not
 *  two independently-typed copies. Falls back to the id when the roster
 *  doesn't carry it (an older daemon, or an id gone stale). */
export function harnessDisplayNames(status: SetupStatus | null | undefined, ids: string[]): string {
  return ids.map((id) => status?.harnesses?.find((h) => h.id === id)?.display || id).join(" and ");
}

/** One provider the strip speaks for (design §5.5, packet MP-D). */
export interface ProviderAttention {
  provider: SetupModelProvider;
  /** provider_access's state: not_configured, expired_signin or expiring. */
  state: string;
  deadline: string;
  /** The agents whose default it is, among those the person may run — the
   *  "{Claude Code} runs use …" of B1, B4 and B5. */
  defaultFor: string[];
}

/**
 * providerAttention is what needs the person, per provider (§5.5): (i) the
 * default provider of an agent they may run, when their credential for it is
 * not connected; and (ii) any provider where they HOLD a credential that is
 * expiring or no longer works. A provider they never used, that is no default,
 * never raises the strip. Empty with no provider block — the legacy strip
 * (modelAccessDoor) speaks there.
 *
 * A Claude subscription's `expiring` (the 11-month aging heuristic) is left to
 * Getting started's row: packet D draws no strip state for it.
 */
export function providerAttention(status: SetupStatus | null | undefined): ProviderAttention[] {
  const out: ProviderAttention[] = [];
  for (const p of status?.model_providers ?? []) {
    const access = status?.provider_access?.find((a) => a.provider === p.id);
    if (p.disabled || !access) continue;
    const defaultFor = (p.default_for ?? []).filter((h) => p.harnesses.includes(h));
    const held = p.kind === "bedrock_sso" && (access.state === "expiring" || access.state === "expired_signin");
    const missing = defaultFor.length > 0 && MODEL_ACCESS_ACTIONABLE.has(access.state) && !(p.kind === "anthropic_subscription" && access.state === "expiring");
    if (held || missing) out.push({ provider: p, state: access.state, deadline: access.deadline ?? "", defaultFor });
  }
  return out;
}

// THE DOOR'S KEY (#544, design §5.9): which door an entrance opens. An entrance
// that knows its provider names it; one that predates providers names today's
// login lane, and resolveDoor keys that to a provider where the install has
// them.
export type DoorRequest = { provider: string } | { login: "aws" | "anthropic" };

// What the one mount renders. `legacy` is today's door (POST
// /setup/harness-login): the only door on an install with no model providers,
// and in the Admin view. `signin` and `key` are the provider doors, User view
// only (packet E: "mounted in the Member view only") — a person's credential
// for a provider is theirs as a person, added from the User view, which is
// also where the server's refusal of the legacy door sends an admin.
export type DoorTarget =
  | { kind: "legacy"; login: "aws" | "anthropic" }
  | { kind: "signin"; login: "aws" | "anthropic"; provider: SetupModelProvider }
  | { kind: "key"; provider: SetupModelProvider; token: boolean; stored: boolean };

const SIGN_IN_KINDS: Record<string, "aws" | "anthropic"> = {
  bedrock_sso: "aws",
  anthropic_subscription: "anthropic",
};

function providerDoor(status: SetupStatus, p: SetupModelProvider): DoorTarget {
  const login = SIGN_IN_KINDS[p.kind];
  if (login) return { kind: "signin", login, provider: p };
  // Every other kind is a typed key or token (the server's providerTypedKinds);
  // custom_endpoint says "token", the rest "key" — gradeProviderKey's own rule.
  const state = status.provider_access?.find((a) => a.provider === p.id)?.state;
  return { kind: "key", provider: p, token: p.kind === "custom_endpoint", stored: state === "live" };
}

/**
 * resolveDoor keys one entrance's request to the door it opens, or null when
 * there is none for it (a provider this person cannot see, or any provider
 * door in the Admin view).
 *
 * A `login` request on an install with providers opens the provider door of
 * that sign-in kind — the claude-code default when it is one, else the first —
 * because it is the only door the server answers there (POST
 * /setup/harness-login refuses while a provider block exists). With no provider
 * of that kind it stays today's door, whose refusal the pane shows verbatim.
 */
export function resolveDoor(
  status: SetupStatus | null | undefined,
  request: DoorRequest,
  view: "admin" | "user",
): DoorTarget | null {
  const providers = (view === "user" && status?.model_providers) || [];
  if ("provider" in request) {
    const p = providers.find((v) => v.id === request.provider);
    return p && status ? providerDoor(status, p) : null;
  }
  const ofKind = providers.filter((p) => !p.disabled && SIGN_IN_KINDS[p.kind] === request.login);
  const p = ofKind.find((v) => v.default_for?.includes(MODEL_ACCESS_AGENT)) ?? ofKind[0];
  return p && status ? providerDoor(status, p) : { kind: "legacy", login: request.login };
}
