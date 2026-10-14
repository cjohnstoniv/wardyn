/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// THE model-access predicate — one answer to "is there something about this
// person's model credential that they should be told about, and can they fix
// it?", read by every surface that offers the sign-in (the shell strip, the New
// Run rail, a credential-failed run's failure block, a held run's approval row).
//
// Since 0.8 (#548) the only answer is per model provider: provider_access,
// one row per provider, graded against the person's own credential.
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
import type { SetupModelProvider, SetupProviderAccess, SetupStatus } from "./types";

// U-10: the per_user "something actionable to do" states — the member's own
// sign-in. Defined once so the Agents tab (admin), member Getting Started
// (member) and the shell strip share ONE policy instead of independently-typed
// literal sets that could drift on a sixth state.
export const MODEL_ACCESS_ACTIONABLE = new Set(["not_configured", "expired_signin", "expiring"]);

// The agent the strip's login-lane requests key to (resolveDoor): a login
// request opens the sign-in provider that is this agent's default when there
// is one.
export const MODEL_ACCESS_AGENT = "claude-code";

/** "Claude Code" / "Claude Code and Codex CLI" — the display names of the
 *  given harness ids, off the roster's own names. Shared by the strip (B1,
 *  B4, B5) and Your model connections' own "For …" line (§5.4): one join, not
 *  two independently-typed copies. Falls back to the id when the roster
 *  doesn't carry it (an older daemon, or an id gone stale). */
export function harnessDisplayNames(status: SetupStatus | null | undefined, ids: string[]): string {
  return ids.map((id) => status?.harnesses?.find((h) => h.id === id)?.display || id).join(" and ");
}

/** One provider the strip speaks for (design §5.5, packet MP-D). */
export interface ProviderAttention extends Pick<SetupProviderAccess, "cause" | "new_destination"> {
  provider: SetupModelProvider;
  /** provider_access's state: not_configured, expired_signin or expiring. */
  state: string;
  deadline: string;
  /** provider_access's action, composed by the server; "" when it has none. */
  action: string;
  /** The agents whose default it is, among those the person may run — the
   *  "{Claude Code} runs use …" of B1, B4 and B5. */
  defaultFor: string[];
}

/**
 * providerAttention is what needs the person, per provider (§5.5): (i) the
 * default provider of an agent they may run, when their credential for it is
 * not connected; and (ii) any provider where they HOLD a credential that is
 * expiring or no longer works. A provider they never used, that is no default,
 * never raises the strip. Empty with no provider block.
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
    if (held || missing)
      out.push({ ...access, provider: p, deadline: access.deadline ?? "", action: access.action ?? "", defaultFor });
  }
  return out;
}

// THE DOOR'S KEY (#544, design §5.9): which door an entrance opens. An entrance
// that knows its provider names it; one that names a login lane is keyed to the
// provider of that sign-in kind (resolveDoor).
export type DoorRequest = { provider: string } | { login: "aws" | "anthropic" };

// What the one mount renders: a provider's sign-in or a typed key, User view
// only (packet E: "mounted in the Member view only") — a person's credential
// for a provider is theirs as a person, added from the User view.
export type DoorTarget =
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
 * there is none for it (a provider this person cannot see, no provider of the
 * requested sign-in kind, or any door in the Admin view).
 *
 * A `login` request opens the provider door of that sign-in kind — the
 * claude-code default when it is one, else the first enabled one.
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
  return p && status ? providerDoor(status, p) : null;
}
