/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The New Run rail's Credentials section: where the model credential lands
// (CredentialFacts) and, with a provider block, which provider the run uses
// (ModelProviderSection, #542). Split out of new-run-rail.tsx by seam when
// #542 took that file past the 1000-line gate. Like the rail, it takes props
// and renders; it owns no screen state.

import * as React from "react";
import type { ModelCredential, SetupModelProvider, SetupProviderAccess } from "../../../lib/types";
import { Button } from "../../ui/button";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../../ui/select";
import { Chip } from "../../wardyn/primitives";
import { AGENTS } from "../../../lib/workspace-providers-copy";
import { RAIL_CREDENTIAL, RAIL_PROVIDER } from "../../wardyn/copy";
import { CONNECTIONS } from "../../wardyn/copy/door";
import {
  accessStateFor,
  providerConnected,
  providerOptionLabel,
  providerResidency,
  type ProviderGate,
} from "./model-provider-lane";

// CredentialFacts states where the model credential lands, and nothing wider —
// "Credentials" as a heading over "never written into the sandbox" was a
// universal claim only the model credential ever supported.
//
// A current preflight verdict describes the exact body about to be launched,
// its chosen provider and all, so it wins; otherwise the answer is unresolved,
// and says so: there is no rung that guesses.
export function CredentialFacts({ cred, preflightRun }: { cred?: ModelCredential; /** Whether a current preflight verdict is on screen. */ preflightRun: boolean }) {
  if (cred) {
    return (
      <>
        <CredentialLine>{credentialSentence(cred)}</CredentialLine>
        {cred.residency === "sandbox" && <AWSSignInChip />}
      </>
    );
  }
  return (
    <>
      <CredentialLine>{RAIL_CREDENTIAL.RESOLVED_AT_LAUNCH}</CredentialLine>
      {/* …and the way to find out, only while there is nothing to find it
          in. A current verdict that carries no `model_credential` (a run no
          provider serves) puts this hint beside the result of pressing it: a
          promise that is false the moment it is followed. */}
      {!preflightRun && <CredentialLine>{RAIL_CREDENTIAL.RUN_PREFLIGHT_HINT}</CredentialLine>}
    </>
  );
}

function CredentialLine({ children }: { children: React.ReactNode }) {
  return <p className="mt-1.5 text-xs leading-relaxed text-muted-foreground first:mt-0">{children}</p>;
}

// Whose AWS sign-in is resident — the person's own; since 0.8 every provider
// credential is (D3 — no shared credential exists to contrast it with).
function AWSSignInChip() {
  return (
    <div className="mt-1.5">
      <Chip tone="neutral">{RAIL_CREDENTIAL.SANDBOX_BEDROCK_CHIP_PER_USER}</Chip>
    </div>
  );
}

// credentialSentence maps a resolved grade to the one sentence true of it.
function credentialSentence(cred: ModelCredential): string {
  switch (cred.residency) {
    case "proxy":
      return RAIL_CREDENTIAL.PROXY;
    case "sandbox":
      // The only kind that grades `sandbox`: a Bedrock AWS sign-in.
      return RAIL_CREDENTIAL.SANDBOX_BEDROCK;
    default:
      return RAIL_CREDENTIAL.RESOLVED_AT_LAUNCH;
  }
}

// #542 — the residency line for a CHOSEN provider (R1, R4), read off its kind
// alone rather than a preflight verdict: with a provider block, every kind's
// residency is known before launch (model-provider-lane.ts's
// providerResidency; packet C's own R4 grouping). The chip stays because
// every provider credential in 0.8 is per person (D3 — no shared credential
// exists to contrast it with).
function ProviderResidencyLine({ provider }: { provider: SetupModelProvider }) {
  if (providerResidency(provider.kind) === "sandbox") {
    return (
      <>
        <CredentialLine>{RAIL_CREDENTIAL.SANDBOX_BEDROCK}</CredentialLine>
        <AWSSignInChip />
      </>
    );
  }
  return <CredentialLine>{RAIL_CREDENTIAL.PROXY}</CredentialLine>;
}

// R3 — the selected candidate isn't connected yet: Launch stays enabled (a
// 422 opens this same door and relaunches, #543), but the person is told
// before pressing it rather than only after. Drawn for all five provider
// kinds providerWhatWord knows (bedrock_sso, custom_endpoint were PR #1036's;
// anthropic_api_key/openai_api_key/anthropic_subscription are the #542
// rail-gap packet's, canon.md's "R3 — selected, not connected"). `state`
// "not_applicable" (the shared admin token — provider_access.go's
// providerAccessMechanism) is not "connected" but is also not a person who
// could ever sign in or add a key, so it renders nothing rather than a door
// that can never work (#542 review finding F3).
function ProviderNotConnectedLine({
  provider,
  access,
  onSignIn,
}: {
  provider: SetupModelProvider;
  access: SetupProviderAccess[] | undefined;
  onSignIn: () => void;
}) {
  const state = accessStateFor(access, provider.id);
  if (providerConnected(state) || state === "not_applicable") return null;
  const name = provider.name ?? provider.id;
  if (provider.kind === "bedrock_sso") {
    return (
      <div className="mt-1.5">
        <p className="text-xs text-warning">{RAIL_PROVIDER.NOT_SIGNED_IN(name)}</p>
        <Button type="button" variant="outline" size="sm" className="mt-1.5" onClick={onSignIn}>
          {AGENTS.SIGN_IN_AWS}
        </Button>
      </div>
    );
  }
  if (provider.kind === "custom_endpoint") {
    return (
      <div className="mt-1.5">
        <p className="text-xs text-warning">{RAIL_PROVIDER.NO_TOKEN(name)}</p>
        <Button type="button" variant="outline" size="sm" className="mt-1.5" onClick={onSignIn}>
          {CONNECTIONS.ADD_TOKEN}
        </Button>
      </div>
    );
  }
  if (provider.kind === "anthropic_api_key" || provider.kind === "openai_api_key") {
    return (
      <div className="mt-1.5">
        <p className="text-xs text-warning">{RAIL_PROVIDER.NO_KEY(name)}</p>
        <Button type="button" variant="outline" size="sm" className="mt-1.5" onClick={onSignIn}>
          {CONNECTIONS.ADD_KEY}
        </Button>
      </div>
    );
  }
  if (provider.kind === "anthropic_subscription") {
    return (
      <div className="mt-1.5">
        <p className="text-xs text-warning">{RAIL_PROVIDER.NOT_SIGNED_IN_CLAUDE(name)}</p>
        <Button type="button" variant="outline" size="sm" className="mt-1.5" onClick={onSignIn}>
          {CONNECTIONS.SIGN_IN_CLAUDE}
        </Button>
      </div>
    );
  }
  return null;
}

// #542 — the Credentials section's provider half: R1 (one candidate, no
// picker — QC-1) through R3/R6/R7/R8 (a select, each option stating what the
// person provides and whether theirs is connected — QC-2), plus the rail-gap
// packet's R5c (`gate`, owner-approved 2026-09-25; R5b is not drawn — see
// providerGate's own doc comment). Rendered instead of CredentialFacts
// whenever the picked agent has at least one candidate OR a gate to name;
// RunRail falls back to CredentialFacts/showModelWarning for everything else
// (no provider block, or none serving this agent at all — R9).
export function ModelProviderSection({
  candidates,
  access,
  selectedId,
  onChange,
  changeNote,
  onSignIn,
  gate,
  harnessLabel,
}: {
  candidates: SetupModelProvider[];
  access: SetupProviderAccess[] | undefined;
  selectedId: string | undefined;
  onChange: (id: string) => void;
  changeNote: string | null;
  onSignIn: (provider: SetupModelProvider) => void;
  /** R5b/R5c (#1052, #542 rail-gap packet) — model-provider-lane.ts's
   *  providerGate. */
  gate?: ProviderGate;
  harnessLabel: string;
}) {
  // R5b — no provider serves this person for this agent at all: no select
  // renders, Launch stays refused via RunRail's `problem`.
  if (gate?.kind === "not_granted") {
    return <p className="text-xs text-warning">{RAIL_PROVIDER.NOT_GRANTED(harnessLabel)}</p>;
  }
  // R5c, no other candidate: no select renders at all, naming the disabled
  // default (Launch stays refused via RunRail's `problem`).
  if (gate?.kind === "default_off" && candidates.length === 0) {
    const name = gate.provider.name ?? gate.provider.id;
    return <p className="text-xs text-warning">{RAIL_PROVIDER.DEFAULT_OFF_ONLY(name, harnessLabel)}</p>;
  }
  // R1 — QC-1: one candidate needs no question with only one answer. Skipped
  // under R5c-with-others (gate set): the sole survivor is NOT the admin's
  // intended default, so it still gets an explicit ask, never a silent STATIC.
  if (!gate && candidates.length === 1) {
    const p = candidates[0];
    return (
      <>
        <p className="text-body font-medium text-foreground">{RAIL_PROVIDER.STATIC(p.name ?? p.id)}</p>
        <ProviderResidencyLine provider={p} />
        <ProviderNotConnectedLine provider={p} access={access} onSignIn={() => onSignIn(p)} />
      </>
    );
  }
  const selected = candidates.find((p) => p.id === selectedId);
  return (
    <>
      <label className="text-xs text-muted-foreground" htmlFor="nr-model-provider">
        {RAIL_PROVIDER.LABEL}
      </label>
      <Select value={selectedId ?? ""} onValueChange={onChange}>
        <SelectTrigger id="nr-model-provider" aria-label={RAIL_PROVIDER.LABEL} className="mt-1">
          <SelectValue placeholder={RAIL_PROVIDER.PLACEHOLDER} />
        </SelectTrigger>
        <SelectContent>
          {candidates.map((p) => (
            <SelectItem key={p.id} value={p.id}>
              {providerOptionLabel(p, access)}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
      {/* R5c, other candidates remain: named until an explicit pick lands —
          selected clears it the same way it clears R7's changeNote below. */}
      {gate?.kind === "default_off" && !selected && (
        <p className="mt-1.5 text-xs text-warning">
          {RAIL_PROVIDER.DEFAULT_OFF(gate.provider.name ?? gate.provider.id, harnessLabel)}
        </p>
      )}
      {/* R7 — QC-3: a change the person didn't make is said once, and clears
          on the next one (the screen drops it as soon as the selection changes). */}
      {changeNote && (
        <p className="mt-1.5 rounded-md border border-info/25 bg-info-subtle px-2 py-1.5 text-xs text-info">
          {changeNote}
        </p>
      )}
      {selected && <ProviderNotConnectedLine provider={selected} access={access} onSignIn={() => onSignIn(selected)} />}
      {selected && <ProviderResidencyLine provider={selected} />}
    </>
  );
}
