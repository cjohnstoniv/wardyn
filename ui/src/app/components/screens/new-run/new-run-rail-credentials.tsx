/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New Run's model-credential facts: where the credential lands
// (CredentialFacts), the rail's summary of the provider the run uses
// (ProviderSummary) and the Run panel's picker for it (ModelProviderSection,
// #542). Split out of new-run-rail.tsx by seam when
// #542 took that file past the 1000-line gate. Like the rail, it takes props
// and renders; it owns no screen state.

import { modelConnectionCause } from "../../../lib/model-connection-cause";
import { useModelAccessDoor } from "../../wardyn/model-access-context";
import * as React from "react";
import type { ModelCredential, SetupModelProvider, SetupProviderAccess } from "../../../lib/types";
import { Button } from "../../ui/button";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../../ui/select";
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
    return <CredentialLine>{credentialSentence(cred)}</CredentialLine>;
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
// providerResidency; packet C's own R4 grouping).
function ProviderResidencyLine({ provider }: { provider: SetupModelProvider }) {
  return (
    <CredentialLine>
      {providerResidency(provider.kind) === "sandbox" ? RAIL_CREDENTIAL.SANDBOX_BEDROCK : RAIL_CREDENTIAL.PROXY}
    </CredentialLine>
  );
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
// What a not-connected provider of each kind says, and the door that connects it.
function notConnected(provider: SetupModelProvider, access: SetupProviderAccess[] | undefined): { sentence: string; cta: string } | null {
  const state = accessStateFor(access, provider.id);
  if (providerConnected(state) || state === "not_applicable") return null;
  const name = provider.name ?? provider.id;
  let sentence: string;
  let cta: string;
  switch (provider.kind) {
    case "bedrock_sso":
      sentence = RAIL_PROVIDER.NOT_SIGNED_IN(name); cta = AGENTS.SIGN_IN_AWS; break;
    case "custom_endpoint":
      sentence = RAIL_PROVIDER.NO_TOKEN(name); cta = CONNECTIONS.ADD_TOKEN; break;
    case "anthropic_api_key":
    case "openai_api_key":
      sentence = RAIL_PROVIDER.NO_KEY(name); cta = CONNECTIONS.ADD_KEY; break;
    case "anthropic_subscription":
      sentence = RAIL_PROVIDER.NOT_SIGNED_IN_CLAUDE(name); cta = CONNECTIONS.SIGN_IN_CLAUDE; break;
    default:
      return null;
  }
  const cause = modelConnectionCause(provider, access?.find((a) => a.provider === provider.id));
  return { sentence: cause?.line ?? sentence, cta: cause?.button ?? cta };
}

function ProviderNotConnectedLine({
  provider,
  access,
  onSignIn,
}: {
  provider: SetupModelProvider;
  access: SetupProviderAccess[] | undefined;
  onSignIn: () => void;
}) {
  const door = useModelAccessDoor();
  const nc = notConnected(provider, access);
  if (!nc) return null;
  return (
    <div className="mt-1.5">
      <p className="text-xs text-warning">{nc.sentence}</p>
      <Button type="button" variant="outline" size="sm" className="mt-1.5" onClick={() => {
        const current = access?.find((a) => a.provider === provider.id);
        if (current?.state === "not_configured" && current.cause === "store_unreadable") void door.refresh();
        else onSignIn();
      }}>
        {nc.cta}
      </Button>
    </div>
  );
}

/** The provider the rail names for this run: the picked one, or the only
 *  candidate when there is no question to ask (R1). */
export function summaryProvider(m: { candidates: SetupModelProvider[]; selectedId: string | undefined; gate?: ProviderGate }): SetupModelProvider | undefined {
  if (!m.gate && m.candidates.length === 1) return m.candidates[0];
  return m.candidates.find((p) => p.id === m.selectedId);
}

// The rail's half of the provider facts, now that the picker sits on the Run
// panel: which provider, where its credential lands, and whether this person
// has connected it. It states; the Run panel asks and opens the door. A
// not-connected provider never holds Launch — the launch door asks then.
export function ProviderSummary({ provider, access }: { provider: SetupModelProvider; access: SetupProviderAccess[] | undefined }) {
  const nc = notConnected(provider, access);
  return (
    <>
      <p className="text-body font-medium text-foreground">{RAIL_PROVIDER.STATIC(provider.name ?? provider.id)}</p>
      <ProviderResidencyLine provider={provider} />
      {nc && <p className="mt-1.5 text-xs text-warning">{nc.sentence}</p>}
    </>
  );
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
