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
import { CC_META } from "../../wardyn/cc-meta";
import { minimalSpec, type PolicyMode } from "../../wardyn/policy-panel";
import { specToSource } from "../../wardyn/policy-document/policy-source";
import type { SetupModelProvider } from "../../../lib/types";
import type { WizardAgent, WizardState } from "./wizard-types";

// The body a fresh Custom policy opens with: a valid, editable floor rather than a blank document
// nobody can start from. Always CC1: there is no persisted operator default
// left to seed this from (the server now picks the strongest installed class
// at or above the floor), and CC1 is the one floor every host can build, so
// the document this opens with is never itself the reason a fresh Custom
// edit can't launch. Written as YAML, the format the editor opens in.
export function defaultSpecText(providers?: readonly SetupModelProvider[]): string {
  return specToSource({ ...minimalSpec(providers), min_confinement_class: "CC1" });
}

// codex-cli has no external tool-approval contract (buildSpec already
// refuses to emit tool_approvals=hold for it; the Seg option renders
// disabled). DERIVED for display only, never patched into state: an effect
// that wrote "auto" back into state.toolApprovals on agent switch would
// clobber a deliberate "hold" choice the moment the operator switched back.
export function effectiveToolApprovals(
  agent: WizardAgent,
  toolApprovals: WizardState["toolApprovals"],
): WizardState["toolApprovals"] {
  return agent === "codex-cli" ? "auto" : toolApprovals;
}

// The Barrier control's per-tier state: which classes actually qualify for
// THIS run (installed AND at or above the active floor), which are merely
// uninstalled, and which are installed but below the floor — one reason per
// tier, never two. `qualifying` is null, never
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

// #1200 — the ACTIVE floor a run is clamped to is never just the authored
// policy's own min_confinement_class: composer.Clamp (clamp.go) raises it to
// the caller's governance ceiling floor whenever THAT ranks higher
// (internal/composer/clamp.go:138-141). A client-side floor that only read
// the authored spec would let the Barrier control offer a tier the server
// then 422s at launch. Either half may be absent (no floor authored, no
// governance profile assigned); undefined only when both are.
export function combineFloors(
  authored: ConfinementClass | undefined,
  governance: ConfinementClass | undefined,
): ConfinementClass | undefined {
  if (!authored) return governance;
  if (!governance) return authored;
  return ccRank(governance) > ccRank(authored) ? governance : authored;
}

// #1200 review R2-1 — "set by your admin" is a claim that the governance
// floor REMOVED a tier this host has: some installed tier ranks below the
// governance floor yet at or above the run's own authored floor (so the
// authored floor alone would have kept it). /policies/default always returns
// a floor (the deployment default, usually CC1, for an unassigned member),
// so comparing the two floors is not enough. Unknown availability claims
// nothing.
export function governanceRemovedTier(
  installed: ConfinementClass[] | null,
  authored: ConfinementClass | undefined,
  governance: ConfinementClass | undefined,
): boolean {
  if (!installed || !governance) return false;
  return installed.some(
    (c) => ccRank(c) < ccRank(governance) && (!authored || ccRank(c) >= ccRank(authored)),
  );
}

// #1200 — T-9's one line naming the requirement, for the Barrier control's
// TierPicker when NOTHING qualifies: whichever of the two barrierReasons
// buckets actually contains the floor tier decides the honest cause — it
// either isn't installed at all, or every tier this host DOES have is weaker
// than the floor. Never both; barrierReasons keeps the two lists disjoint.
export function barrierRequirementReason(
  effectiveFloor: ConfinementClass,
  unavailable: ConfinementClass[],
  belowFloor: ConfinementClass[],
  // #1200 review P2-6 — the SAME honest /dev/kvm reason
  // environment-step.tsx's own picker computes (vaultIncompatibleReason),
  // passed in by the caller when it has a SetupStatus to compute it from.
  // Preferred over the generic "isn't installed" line whenever the missing
  // tier IS Vault, since that's the one case with a comparable hardware fact
  // to name instead of a bare "not installed".
  vaultReason?: string,
): string {
  if (effectiveFloor === "CC3" && vaultReason) return vaultReason;
  if (unavailable.includes(effectiveFloor)) {
    return `${CC_META[effectiveFloor].label} isn't installed on this host.`;
  }
  if (belowFloor.length > 0) {
    return `Every barrier installed on this host is below ${CC_META[effectiveFloor].label}.`;
  }
  return `${CC_META[effectiveFloor].label} isn't installed on this host.`;
}

// A saved-policy id that no longer resolves (the policy was deleted
// elsewhere) must say so rather than silently falling through to "no problem".
// Gated on policiesLoaded too: an id carried in from a clone prefill has no
// match on the very first render, before listPolicies() has answered, and that
// is "unknown", never "gone".
export function savedPolicyGone(
  policyMode: PolicyMode,
  selectedPolicyId: string | undefined,
  selectedPolicy: unknown,
  policiesLoaded: boolean,
): boolean {
  return policyMode === "saved" && !!selectedPolicyId && !selectedPolicy && policiesLoaded;
}
