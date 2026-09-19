/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New Run's Policy-lane helpers — split out because new-run-screen.tsx sits at
// the file-size gate's ceiling (scripts/check-file-size.sh, 1000 lines): every
// piece of NEW logic this lane adds lands here instead, as a pure function the
// screen calls. React state stays in the screen; this only answers questions
// about it.
import { CC_ORDER, type ConfinementClass } from "../../../lib/types";
import { ccRank } from "./new-run-primitives";
import { POLICY_TEMPLATES } from "../../wardyn/policy-panel";
import type { WizardAgent, WizardState } from "./wizard-types";

const MINIMAL = POLICY_TEMPLATES.find((t) => t.id === "minimal")!;

// The body a fresh Custom policy opens with: a valid, editable floor rather
// than a blank document nobody can start from. Also what F2-F1 resets TO.
// Always CC1 (0.7.8): there is no persisted operator default left to seed
// this from (the server now picks the strongest installed class at or above
// the floor), and CC1 is the one floor every host can build, so the document
// this opens with is never itself the reason a fresh Custom edit can't launch.
export function defaultSpecText(): string {
  return JSON.stringify({ ...MINIMAL.spec, min_confinement_class: "CC1" }, null, 2);
}

// F2-F1 — a saved-policy body comes back REDACTED for anyone who is NOT
// security-tier (redactPoliciesForRead, gated server-side on isSecurityOperator
// — admin OR security_admin, internal/api/http.go — not the narrower isOperator
// admin-only predicate). Picking one loads that redacted JSON into the
// textarea; switching lanes to Custom used to leave it sitting there, so
// launching inline shipped a policy with a dead credential reference nobody
// authored.
//
// R1 (post-ship review): the caller must pass `securityOperator && resolved`,
// NEVER the bare `operator`/`securityOperator` context booleans — both default
// fail-OPEN (true while /me is unresolved or the fetch failed,
// operator-context.tsx), which is backwards for a clear that has to fire even
// when a MEMBER's /me hasn't answered yet. A security_admin's own real body
// (never redacted) is the other edge this gate must recognise, which is why it
// is NOT the admin-only `operator` either.
export function clearedSpecOnCustomSwitch(
  active: boolean,
  keepsRealBody: boolean,
  hadSelection: boolean,
): string | undefined {
  if (active || keepsRealBody || !hadSelection) return undefined;
  return defaultSpecText();
}

// F2-F4 — codex-cli has no external tool-approval contract (buildSpec already
// refuses to emit tool_approvals=hold for it; the Seg option renders disabled).
// DERIVED for display only, never patched into state: an effect that wrote
// "auto" back into state.toolApprovals on agent switch would clobber a
// deliberate "hold" choice the moment the operator switched back.
export function effectiveToolApprovals(
  agent: WizardAgent,
  toolApprovals: WizardState["toolApprovals"],
): WizardState["toolApprovals"] {
  return agent === "codex-cli" ? "auto" : toolApprovals;
}

// The Barrier control's per-tier state (item 3, 0.7.8): which classes
// actually qualify for THIS run (installed AND at or above the active
// floor), which are merely uninstalled, and which are installed but below
// the floor — one reason per tier, never two. `qualifying` is null, never
// [], when availability itself is unknown, so a caller can tell "nothing
// qualifies" (fail-closed, floor above every buildable tier) apart from
// "we don't know yet" (never renders the single-qualifier sentence, and
// never disables a tier on a guess).
export interface BarrierReasons {
  qualifying: ConfinementClass[] | null;
  unavailable: ConfinementClass[];
  belowFloor: ConfinementClass[];
}

export function barrierReasons(
  available: ConfinementClass[] | null,
  floor: ConfinementClass | undefined,
): BarrierReasons {
  if (!available) return { qualifying: null, unavailable: [], belowFloor: [] };
  return {
    qualifying: CC_ORDER.filter((c) => available.includes(c) && (!floor || ccRank(c) >= ccRank(floor))),
    unavailable: CC_ORDER.filter((c) => !available.includes(c)),
    belowFloor: floor ? CC_ORDER.filter((c) => available.includes(c) && ccRank(c) < ccRank(floor)) : [],
  };
}

// F2-F5 — a saved-policy id that no longer resolves (the policy was deleted
// elsewhere) must say so rather than silently falling through to "no problem".
// Gated on policiesLoaded too: an id carried in from a clone prefill has no
// match on the very first render, before listPolicies() has answered, and that
// is "unknown", never "gone".
export function savedPolicyGone(
  useSaved: boolean,
  selectedPolicyId: string | undefined,
  selectedPolicy: unknown,
  policiesLoaded: boolean,
): boolean {
  return useSaved && !!selectedPolicyId && !selectedPolicy && policiesLoaded;
}
