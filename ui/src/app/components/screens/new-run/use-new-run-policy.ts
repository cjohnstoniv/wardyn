/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New Run's policy-derivation hook — split out of new-run-screen.tsx (the
// file's own 1000-line gate, scripts/check-file-size.sh): the floor a run is
// clamped to, what qualifies on this host, the post-parse merge, the rail's
// tool-rules line and the ADO launch door all live here as ONE hook, so the
// barrier's active floor and the rail's own description of it can never be
// computed twice and drift apart. It owns two effects (re-deriving the
// authored floor on every parse, and up-clamping the Barrier Seg to the
// active floor) — that is why this is a hook rather than a pure function like
// policy-lane.ts's. React state (specText, parsedFloor, useSaved, …) stays
// owned by NewRunScreen: the hook derives from it and writes back only through
// its two effects (setParsedFloor, patch, pristineCc).
import * as React from "react";
import {
  CC_ORDER as ORDERED_CLASSES,
  type ConfinementClass,
  type RunPolicySpec,
  type SetupModelProvider,
  type Workspace,
} from "../../../lib/types";
import { ccRank as rank } from "./new-run-primitives";
import { useAdoLaunchDoor } from "./new-run-rail";
import { parseSpec, toolRulesSummary, unparseableFloorClass } from "../../wardyn/policy-panel";
import { barrierReasons, combineFloors, governanceRemovedTier } from "./policy-lane";
import { mergeRunSelections } from "./wizard-spec";
import type { WizardState } from "./wizard-types";

export interface UseNewRunPolicyParams {
  state: WizardState;
  patch: (p: Partial<WizardState>) => void;
  useSaved: boolean;
  specText: string;
  parsedFloor: ConfinementClass | undefined;
  setParsedFloor: React.Dispatch<React.SetStateAction<ConfinementClass | undefined>>;
  savedPolicies: { id: string; name: string; spec: RunPolicySpec }[];
  availableClasses: ConfinementClass[] | null;
  probeSettled: boolean;
  governanceProfile: string | undefined;
  govFloor: ConfinementClass | undefined;
  operator: boolean;
  workspaces: Workspace[];
  modelProviders?: SetupModelProvider[];
  /** The Barrier's dirty-check baseline (new-run-screen.tsx's pristineCc, next
   *  to pristineSpec) — the up-clamp effect below moves it with its own write,
   *  as the /setup/status effect does when it re-seeds the class, so a
   *  machine-made clamp never reads as an operator edit. */
  pristineCc: React.MutableRefObject<ConfinementClass | undefined>;
}

export function useNewRunPolicy({
  state,
  patch,
  useSaved,
  specText,
  parsedFloor,
  setParsedFloor,
  savedPolicies,
  availableClasses,
  probeSettled,
  governanceProfile,
  govFloor,
  operator,
  workspaces,
  modelProviders,
  pristineCc,
}: UseNewRunPolicyParams) {
  const cc = state.confinementClass;
  const parsed = parseSpec(specText);

  // C5's one real trap (policy-panel.tsx's own doc) — the field is present and
  // this build can't spell it.
  const unparseableFloor = unparseableFloorClass(parsed);

  const selectedPolicy =
    useSaved && state.selectedPolicyId
      ? savedPolicies.find((p) => p.id === state.selectedPolicyId)
      : undefined;

  // Every successful parse re-reads the floor the document authors; a FAILED
  // parse changes nothing (parsedFloor stays whatever last parsed).
  React.useEffect(() => {
    const p = parseSpec(specText);
    if (!p.ok) return;
    const f = p.spec.min_confinement_class as ConfinementClass;
    setParsedFloor(ORDERED_CLASSES.includes(f) ? f : undefined);
  }, [specText, setParsedFloor]);

  // The ACTIVE floor: a picked saved policy's stored floor, else the last
  // successful parse's. Both paths refuse to launch below it server-side.
  const floor = useSaved ? (selectedPolicy?.spec.min_confinement_class as ConfinementClass | undefined) : parsedFloor;

  // #1200 review P2-2 — govFloor binds ONLY where the server would actually
  // clamp to it, mirrored exactly from the two doors that decide that:
  //   - an OPERATOR is never clamped at all (effectiveCeiling's own
  //     short-circuit, internal/api/governance.go; an admin's inline policy
  //     specifically, inline_policy.go's "admin ⇒ no clamp");
  //   - the INLINE (custom) lane clamps every non-operator unconditionally —
  //     even unassigned, since the deployment default IS the ceiling then;
  //   - the SAVED-POLICY lane clamps only when a NAMED profile is assigned
  //     (governanceProfile present) — an unassigned member's saved policy is
  //     not raised to the deployment default at all (inline_policy.go:310's
  //     `ceiling.Profile != nil`).
  // Folding it unconditionally (the pre-review build) hid tiers the server
  // would have let an admin, or an unassigned member's saved policy, use.
  const govFloorApplies = !operator && (!useSaved || !!governanceProfile);
  const boundGovFloor = govFloorApplies ? govFloor : undefined;

  // #1200 — the floor that actually binds: whichever of the authored
  // policy's own floor and the (now correctly gated) governance ceiling's
  // ranks HIGHER (combineFloors — composer.Clamp raises a weaker authored
  // floor to the ceiling's, never the other way). A Barrier control that
  // only read `floor` could offer a tier the server then 422s at launch.
  const effectiveFloor = combineFloors(floor, boundGovFloor);

  // #1200 review P2-1/R2-1 — whether the GOVERNANCE ceiling actually removed
  // an installed tier, as opposed to this host simply having one tier, or the
  // run's OWN authored floor doing the narrowing (governanceRemovedTier).
  // Only this case gets TierPicker's "set by your admin" line and the
  // governance-sourced requirement wording.
  const governanceBinding = governanceRemovedTier(availableClasses, floor, boundGovFloor);

  // The Barrier control's per-tier state — see barrierReasons.
  const { qualifying, unavailable, belowFloor } = barrierReasons(availableClasses, effectiveFloor);

  // #214 — a SETTLED probe reporting zero classes: this host genuinely
  // cannot build any barrier, so Launch itself is disabled, not just every
  // tier. Deliberately host-level, not the governance-narrowed empty-qualifying
  // shape TierPicker's own T-9 card already answers below (effectiveFloor set,
  // every installed tier still below it) — that is a policy choice, not a
  // reason Launch can never work here. Also deliberately NOT the unknown-probe
  // case (probeSettled stays false there): unresolved stays selectable, exactly
  // as it already was, unrelated to this issue.
  const noBarrierOnHost = probeSettled && !!availableClasses && availableClasses.length === 0;

  // UP-CLAMP the Barrier Seg to the active floor. `cc` is in the deps on
  // purpose: the /setup/status read resolves ASYNCHRONOUSLY and re-seeds
  // confinementClass from the server's own default, which can land BELOW a
  // floor this already clamped to. Watching the value, not just the floor,
  // makes "never below the floor" an invariant instead of a one-shot.
  React.useEffect(() => {
    if (!effectiveFloor || !ORDERED_CLASSES.includes(effectiveFloor)) return;
    if (rank(effectiveFloor) > rank(cc)) {
      pristineCc.current = effectiveFloor;
      patch({ confinementClass: effectiveFloor });
    }
  }, [effectiveFloor, cc, patch, pristineCc]);

  // The post-parse union, computed ONCE: the same value renders the "Added for
  // this run's selections" line and goes on the wire, so the screen cannot show
  // one policy and launch another.
  const merged = React.useMemo(
    () => (parsed.ok ? mergeRunSelections(parsed.spec, state, workspaces, modelProviders) : null),
    // eslint-disable-next-line react-hooks/exhaustive-deps -- parsed is rebuilt every render; specText is what actually changes
    [specText, state, workspaces, modelProviders],
  );
  const added = merged?.added;

  // #386's launch door — F8: never relaunches.
  const adoDoor = useAdoLaunchDoor();

  // What this run's tool_rules actually say, from the SAME spec that ships:
  // the merged document on the custom lane, the stored one on the saved lane.
  // Null when there are no rules, so a policy written before the field existed
  // grows no empty rail section.
  const specForRules = useSaved ? selectedPolicy?.spec : merged?.spec;
  const toolRules = React.useMemo(() => (specForRules ? toolRulesSummary(specForRules) : null), [specForRules]);

  return {
    parsed,
    unparseableFloor,
    selectedPolicy,
    effectiveFloor,
    governanceBinding,
    qualifying,
    unavailable,
    belowFloor,
    noBarrierOnHost,
    merged,
    added,
    specForRules,
    toolRules,
    adoDoor,
  };
}
