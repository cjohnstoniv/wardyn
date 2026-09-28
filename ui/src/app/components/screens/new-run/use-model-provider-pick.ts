/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #542's provider preselection — split out of new-run-screen.tsx (the file's
// own 1000-line gate), the same seam use-launch.ts and use-ado-launch-door.ts
// already took. The body is the screen's own, moved as it was: which provider
// (if any) is preselected for the current agent, and the person's own pick.
import * as React from "react";
import type { SetupModelProvider, SetupProviderAccess } from "../../../lib/types";
import { agentLabel, type WizardState } from "./wizard-types";
import { providerGate, resolveProviderSelection } from "./model-provider-lane";

export function useModelProviderPick({
  state,
  isAgent,
  modelProviders,
  providerAccess,
  providerCandidates,
  pin,
  providerGateState,
  patch,
}: {
  state: Pick<WizardState, "agent" | "modelProviderId">;
  isAgent: boolean;
  modelProviders: SetupModelProvider[] | undefined;
  providerAccess: SetupProviderAccess[] | undefined;
  providerCandidates: SetupModelProvider[];
  pin: string | undefined;
  providerGateState: ReturnType<typeof providerGate> | undefined;
  patch: (p: Partial<WizardState>) => void;
}) {
  // R7's info line — cleared the moment the person makes their OWN choice
  // (onModelProviderChange below), not just on the next agent switch.
  const [providerChangeNote, setProviderChangeNote] = React.useState<string | null>(null);

  // Opus review round 2 (F2) — whether the CURRENT state.modelProviderId is
  // the person's OWN prior pick (a real onModelProviderChange call) or this
  // effect's own earlier automatic adoption. Read by resolveProviderSelection
  // as `previousExplicit`: keeping ANY still-serving previousId unconditionally
  // — this build's original bug — let an automatic pick outlive a pin that
  // resolved later (the workspace attaching or finishing its load AFTER the
  // providers already have is the ordinary sequence, not an edge case) and
  // ride across an agent switch into an agent whose own default is disabled.
  // Reset to false the moment this effect adopts something itself (see
  // below) or the agent changes — either way the CURRENT selection is no
  // longer something a person is on record as having chosen for THIS context.
  const explicitPick = React.useRef(false);

  // Which provider (if any) is preselected for the CURRENT agent — R1/R2's
  // silent default, R6's silent non-default, R7/R8's agent-switch rule. Runs
  // whenever the candidate set for this agent could have changed: the
  // providers finished loading, a sign-in changed provider_access, or the
  // agent picker moved. Deliberately does NOT depend on state.modelProviderId
  // itself — that would fire the moment this effect's OWN patch() lands and
  // fight a person's manual pick the instant they made it.
  const prevAgentRef = React.useRef(state.agent);
  React.useEffect(() => {
    if (!isAgent || modelProviders === undefined) return;
    const agentChanged = prevAgentRef.current !== state.agent;
    prevAgentRef.current = state.agent;
    const previous = modelProviders.find((p) => p.id === state.modelProviderId);
    const result = resolveProviderSelection({
      candidates: providerCandidates,
      agent: state.agent,
      agentLabel: agentLabel(state.agent),
      previousId: state.modelProviderId,
      previousName: previous?.name ?? previous?.id,
      agentChanged,
      previousExplicit: explicitPick.current,
      pin,
      defaultDisabled: providerGateState?.kind === "default_off",
    });
    setProviderChangeNote(result.changeNote);
    if (result.selectedId !== state.modelProviderId) {
      patch({ modelProviderId: result.selectedId });
      explicitPick.current = false; // an automatic adoption, never an explicit pick
    }
    if (agentChanged) explicitPick.current = false;
    // eslint-disable-next-line react-hooks/exhaustive-deps -- state.modelProviderId deliberately excluded (see comment above); patch is stable
  }, [isAgent, modelProviders, providerAccess, state.agent, pin, providerGateState?.kind]);

  // The person's OWN pick always wins outright and clears R7's note — a
  // choice they just made is never something to explain to them.
  const onModelProviderChange = (id: string) => {
    explicitPick.current = true;
    setProviderChangeNote(null);
    patch({ modelProviderId: id });
  };

  return { providerChangeNote, onModelProviderChange };
}
