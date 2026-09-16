/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New Run's Policy-lane helpers — split out because new-run-screen.tsx sits at
// the file-size gate's ceiling (scripts/check-file-size.sh, 1000 lines): every
// piece of NEW logic this lane adds lands here instead, as a pure function the
// screen calls. React state stays in the screen; this only answers questions
// about it.
import type { ConfinementClass } from "../../../lib/types";
import { getDefaultCc } from "../../wardyn/default-confinement";
import { POLICY_TEMPLATES } from "../../wardyn/policy-panel";
import type { WizardAgent, WizardState } from "./wizard-types";

const MINIMAL = POLICY_TEMPLATES.find((t) => t.id === "minimal")!;

// The body a fresh Custom policy opens with: a valid, editable floor rather
// than a blank document nobody can start from. Also what F2-F1 resets TO.
export function defaultSpecText(cc: ConfinementClass | null): string {
  return JSON.stringify({ ...MINIMAL.spec, min_confinement_class: cc ?? "CC1" }, null, 2);
}

// F2-F1 — a member's saved-policy body comes back REDACTED
// (redactPoliciesForRead: secret refs read as "<redacted>"). Picking one loads
// that redacted JSON into the textarea; switching lanes to Custom used to leave
// it sitting there, so launching inline shipped a policy with a dead credential
// reference nobody authored. Operators see the real body — never redacted — so
// only a non-operator switching AWAY from an actual selection gets cleared;
// an operator's own edits, or a switch with nothing picked, are left alone.
export function clearedSpecOnCustomSwitch(
  active: boolean,
  operator: boolean,
  hadSelection: boolean,
): string | undefined {
  if (active || operator || !hadSelection) return undefined;
  return defaultSpecText(getDefaultCc());
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
