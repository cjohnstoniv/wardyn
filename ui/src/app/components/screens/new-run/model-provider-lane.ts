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
// R5's states (a disabled default, or nobody granted at all) are OUT OF SCOPE
// for this build (owner ruling, 2026-09-25): the packet drew R5b ("Granted
// none") and R5c ("The default is turned off") as their own cards, distinct
// from a plain "R5" this acceptance list never named. Nothing here renders
// either sentence; a disabled or wholly-ungranted default falls through to
// R6's shape (no preselection, Launch waits for a choice) with no extra line
// naming WHY nothing was preselected.
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
 */
export function resolveProviderSelection(params: {
  candidates: SetupModelProvider[];
  agent: string;
  agentLabel: string;
  previousId: string | undefined;
  previousName: string | undefined;
  agentChanged: boolean;
}): ProviderSelectionResult {
  const { candidates, agent, agentLabel, previousId, previousName, agentChanged } = params;
  if (previousId && candidates.some((c) => c.id === previousId)) {
    return { selectedId: previousId, changeNote: null };
  }
  const adopted = candidates.length === 1 ? candidates[0] : defaultCandidate(candidates, agent);
  if (!adopted) return { selectedId: undefined, changeNote: null };
  const changeNote =
    agentChanged && previousId && previousName
      ? RAIL_PROVIDER.CHANGED(adopted.name ?? adopted.id, previousName, agentLabel)
      : null;
  return { selectedId: adopted.id, changeNote };
}
