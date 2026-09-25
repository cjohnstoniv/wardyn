/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #542 (design §5.6, packet MP-C) — the New Run rail's provider picker, as
// pure derivations over /setup/status's `model_providers`/`provider_access`
// (already filtered to what THIS person may use — #1015). Split from
// new-run-rail.tsx and new-run-screen.tsx (both near the file-size gate) and
// from wizard-spec.ts (a different concern: this is candidate/selection
// derivation, not wire-body composition) so the states R1/R2/R3/R6/R7/R8 have
// one tested home each, mirroring policy-lane.ts's split for barrierReasons.
//
// R5b ("Granted none") and R5c ("The default is turned off") were OUT OF
// SCOPE for PR #1036's build (owner ruling, 2026-09-25) — the packet drew them
// as their own cards, distinct from a plain "R5" that build's acceptance list
// never named, and nothing rendered either sentence: a disabled or
// wholly-ungranted default fell through to R6's shape (no preselection,
// Launch waits) with no line naming WHY. The owner approved drawing both
// (docs/design/542-rail-gaps-mock/canon.md, same day), but Opus review round
// 2 found R5b undrawable from here: setupModelProviderState's capVisible
// (internal/api/provider_access.go) already narrows `model_providers` to
// providers THIS PERSON is granted before the wire, so an ungranted provider
// never reaches this module at all, and the server's own chooseModelProvider
// launches on R9's silent advisory in that shape rather than refusing. Only
// R5c ships: providerGate below is read by both resolveProviderSelection (so
// a disabled default is never silently replaced by whichever OTHER candidate
// happened to survive) and the rail's own ModelProviderSection (so it can
// name the same fact instead of falling through to R9's generic shape).
import type { SetupModelProvider, SetupProviderAccess } from "../../../lib/types";
import { RAIL_PROVIDER } from "../../wardyn/copy/new-run-rail";

/** The providers this person may pick for `agent` (§2.4's candidate set,
 *  already access-filtered server-side — this is only the harness/disabled
 *  narrowing the console itself must apply). */
export function providerCandidates(providers: SetupModelProvider[] | undefined, agent: string): SetupModelProvider[] {
  return (providers ?? []).filter((p) => !p.disabled && p.harnesses.includes(agent));
}

/** This person's own connection state for `id`; "not_configured" absent —
 *  same reading rule as every other provider_access consumer (model-access.ts). */
export function accessStateFor(access: SetupProviderAccess[] | undefined, id: string): string {
  return access?.find((a) => a.provider === id)?.state ?? "not_configured";
}

/** Whether a state reads as "there is a working credential right now" —
 *  `live` and `expiring` (still usable, just aging); anything else is nothing
 *  to launch on yet. */
export function providerConnected(state: string): boolean {
  return state === "live" || state === "expiring";
}

function isSignInKind(kind: string): boolean {
  return kind === "bedrock_sso" || kind === "anthropic_subscription";
}

/** The word RAIL_PROVIDER.OPTION fills "your {what}" with, per kind. */
export function providerWhatWord(kind: string): string {
  if (kind === "bedrock_sso") return "AWS sign-in";
  if (kind === "anthropic_subscription") return "sign-in";
  if (kind === "custom_endpoint") return "token";
  return "key";
}

/** The trailing state word ("added"/"not added"/"signed in"/"not signed
 *  in"), keyed on the SAME kind-vocabulary as providerWhatWord. */
export function providerStateWord(kind: string, state: string): string {
  const connected = providerConnected(state);
  if (isSignInKind(kind)) return connected ? "signed in" : "not signed in";
  return connected ? "added" : "not added";
}

/** RAIL_PROVIDER.OPTION for one candidate, given this person's access rows. */
export function providerOptionLabel(p: SetupModelProvider, access: SetupProviderAccess[] | undefined): string {
  const state = accessStateFor(access, p.id);
  return RAIL_PROVIDER.OPTION(p.name ?? p.id, providerWhatWord(p.kind), providerStateWord(p.kind, state));
}

/** The admin's roster default for `agent`, among candidates this person may
 *  actually use — a default outside the candidate set (disabled, or granted
 *  to nobody) is R5's territory (out of scope) and answers undefined here,
 *  same as no default at all. */
export function defaultCandidate(candidates: SetupModelProvider[], agent: string): SetupModelProvider | undefined {
  return candidates.find((p) => p.default_for?.includes(agent));
}

/** R4/R1: where a candidate's credential lives, drawn straight off packet
 *  C's own R4 grouping — "key, token or Claude sign-in" reads one proxy
 *  sentence; AWS sign-in alone reads the sandbox one. Bedrock SSO always
 *  exchanges a live AWS session inside the sandbox; every other kind is a
 *  bearer value the proxy injects. */
export function providerResidency(kind: string): "sandbox" | "proxy" {
  return kind === "bedrock_sso" ? "sandbox" : "proxy";
}

export interface ProviderSelectionResult {
  selectedId: string | undefined;
  /** R7's info line, or null (R1/R2's silent preselect, R6's silent
   *  no-preselect, R8's silent retention). */
  changeNote: string | null;
}

/** All providers serving `agent`, DISABLED ROWS INCLUDED — unlike
 *  providerCandidates above, which is what a person may actually pick.
 *  providerGate needs the wider set to tell a real "nothing serves this
 *  agent" (R9) apart from "something does, but it's disabled" (R5c) — a
 *  disabled row is published, never hidden (SetupModelProvider.disabled's own
 *  doc comment), precisely so this distinction can be drawn. */
export function providersServing(providers: SetupModelProvider[] | undefined, agent: string): SetupModelProvider[] {
  return (providers ?? []).filter((p) => p.harnesses.includes(agent));
}

export type ProviderGate = { kind: "default_off"; provider: SetupModelProvider };

/**
 * R5c (#542 rail-gap packet, owner-approved 2026-09-25): the reason nothing
 * may launch yet when providerCandidates comes back empty (or the sole
 * survivor isn't the intended default) because this agent's own NAMED roster
 * default is a disabled provider — regardless of how many other candidates
 * remain (mirrors chooseModelProvider's own unconditional disabled-default
 * refusal, internal/api/run_model_provider.go: the disabled default is
 * refused even with exactly one other candidate left, never silently passed
 * over). R9's "nothing serves this agent at all" stays this function's
 * `undefined`, its existing silent shape unchanged.
 *
 * R5b ("granted none") is deliberately NOT drawn here (Opus review round 2):
 * setupModelProviderState (internal/api/provider_access.go's capVisible)
 * already narrows `model_providers` to providers THIS PERSON is granted
 * before it ever reaches the wire, so an UNGRANTED provider never appears at
 * all — there is no "all serving rows disabled" shape left for the console to
 * read as "granted none", and the server's own chooseModelProvider agrees:
 * with no named default and zero enabled candidates it launches on R9's
 * silent advisory (run_model_provider.go's `len(serving) == 0` branch), not a
 * refusal. RAIL_PROVIDER.NOT_GRANTED stays defined (canon-pinned) for the
 * follow-up issue that gives the console a real signal for it; nothing here
 * produces it yet.
 */
export function providerGate(providers: SetupModelProvider[] | undefined, agent: string): ProviderGate | undefined {
  const serving = providersServing(providers, agent);
  if (serving.length === 0) return undefined;
  const disabledDefault = serving.find((p) => p.disabled && p.default_for?.includes(agent));
  if (disabledDefault) return { kind: "default_off", provider: disabledDefault };
  return undefined;
}

/**
 * resolveProviderSelection is R2/R6/R7/R8's one shared rule, run whenever the
 * candidate set for the picked agent changes (first arrival, or an agent
 * switch):
 *
 *   - an explicit selection that still serves the new agent is KEPT, silently
 *     (R8 — "Corp gateway" serves both Claude Code and Codex CLI);
 *   - otherwise the agent's own granted default (or the sole candidate) is
 *     adopted — silently on first arrival (R1/R2), with CHANGED named only
 *     when an agent switch is what knocked out a PRIOR explicit selection
 *     (R7);
 *   - with no default among the candidates, nothing is preselected and the
 *     person chooses (R6 — QC-4: Wardyn never silently substitutes).
 *
 * A workspace pin (F2, #612) is checked ahead of the default/sole-candidate
 * rule: a pin naming a candidate is preselected ahead of the roster default,
 * silently; a pin naming a provider that is NOT a candidate preselects
 * nothing at all — the server refuses it by name (cmp.Or(requested, pin) in
 * internal/api/run_model_provider.go), and this rail must not paper over that
 * refusal by silently substituting some OTHER provider the pin never named.
 *
 * `previousId` is kept ONLY when it is either the person's OWN prior pick
 * (`previousExplicit`) or there is nothing that could outrank it (no pin, and
 * the agent's own default isn't disabled) — Opus review round 2: keeping ANY
 * still-serving previousId unconditionally, before ever looking at the pin,
 * let an AUTOMATIC pick (this function's own earlier R1/R2 adoption) outlive
 * a pin that arrived later — the workspace attaching or finishing its load
 * AFTER the providers already resolved is the ordinary sequence, not an edge
 * case — and let that same automatic pick ride across an agent switch into an
 * agent whose own default is disabled, bypassing R5c's "never auto-carried"
 * rule. An EXPLICIT pick still always wins outright, matching the doctrine
 * new-run-screen.tsx's onModelProviderChange already states in prose: a
 * choice the person actually made is never something this rule second-guesses.
 */
export function resolveProviderSelection(params: {
  candidates: SetupModelProvider[];
  agent: string;
  agentLabel: string;
  previousId: string | undefined;
  previousName: string | undefined;
  agentChanged: boolean;
  /** True when `previousId` is the person's OWN prior pick (a real
   *  onModelProviderChange call), never an automatic adoption this function
   *  itself made on an earlier run — see the doc comment above. */
  previousExplicit?: boolean;
  /** The primary workspace's llm_cred.provider_ref, if any — see the doc
   *  comment above. */
  pin?: string;
  /** True when this agent's own named roster default is a disabled provider
   *  (providerGate's "default_off"): the sole-survivor shortcut below must
   *  NOT adopt whichever other candidate is left standing, or a disabled
   *  default would be replaced silently instead of refused (R5c). */
  defaultDisabled?: boolean;
}): ProviderSelectionResult {
  const { candidates, agent, agentLabel, previousId, previousName, agentChanged, pin, defaultDisabled, previousExplicit } =
    params;
  const previousStillServes = !!previousId && candidates.some((c) => c.id === previousId);
  if (previousStillServes && (previousExplicit || (!pin && !defaultDisabled))) {
    return { selectedId: previousId, changeNote: null };
  }
  if (pin) {
    // Rule (1)/(2): a pin always decides the outcome once it's set — a
    // candidate pin wins, a non-candidate pin leaves nothing preselected,
    // and neither case falls through to the roster default below.
    return { selectedId: candidates.find((c) => c.id === pin)?.id, changeNote: null };
  }
  const adopted =
    defaultCandidate(candidates, agent) ?? (candidates.length === 1 && !defaultDisabled ? candidates[0] : undefined);
  if (!adopted) return { selectedId: undefined, changeNote: null };
  const changeNote =
    agentChanged && previousId && previousName
      ? RAIL_PROVIDER.CHANGED(adopted.name ?? adopted.id, previousName, agentLabel)
      : null;
  return { selectedId: adopted.id, changeNote };
}
