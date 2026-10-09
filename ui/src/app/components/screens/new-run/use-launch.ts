/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { useNavigate } from "react-router-dom";
import type { CreateRunResult, PreflightResult } from "../../../lib/types";
import { isCredentialRefusal, runWireBody, runs as runsApi } from "../../../lib/api/runs";
import { getAuthGeneration, HttpError, isSignedOutHold } from "../../../lib/api/core";
import type { PolicyRef } from "../../../lib/api/health";
import { useDeferredBusy } from "../../../lib/use-deferred-busy";
import { getErrorMessage } from "../../../lib/format";
import { buildRunInput, type RunInputParams } from "./build-run-input";
import { useRunChecks, type DraftIdentity } from "./use-run-checks";
export { PREFLIGHT_FRESH_MS, PREFLIGHT_DEBOUNCE_MS } from "./use-run-checks";

export interface UseLaunchParams extends RunInputParams {
  /** Called from Launch's catch block with the caught error (#386's Azure DevOps launch door). */
  onLaunchError?: (e: unknown) => void;
  /** What the screen already knows about whether an automatic preflight may
   *  fire and which preflight-derived rows block Launch. `local` is true only
   *  when Launch would otherwise be pressable on the form's own say-so: a body
   *  exists, launchGates has no problem, and neither workspaceUnavailable nor
   *  noBarrier is set. The arms below read preflight's OWN answer, so they
   *  gate Launch (preflightBlock) and never gate firing a check. */
  autoCheck: { local: boolean; backendArm: boolean; modelArm: boolean };
  /** The sign-in door is open; its closing re-checks the body. */
  doorOpen: boolean;
  adoDoorOpen: boolean;
  identity: DraftIdentity;
  externalRevision: string;
  sourceRefreshPending: boolean;
  onCreated: () => void;
}

export interface UseLaunchResult extends ReturnType<typeof useRunChecks> {
  launching: boolean;
  launchDisabled: boolean;
  launchSpinning: boolean;
  error: string | null;
  /** Bumped on every failed launch, including a repeat of the same message —
   *  so the rail's alert region remounts and gets re-announced (#459). */
  errorSeq: number;
  /** The policy the refusal came from and how to ask for a change (the
   *  envelope's `policy`), undefined when the failure carries none. */
  errorPolicy: PolicyRef | undefined;
  credentialRefused: boolean;
  /** The model provider that credential refusal names (#532), "" when it
   *  names none — the door the rail opens is THAT provider's (#543). */
  refusedProvider: string;
  /** Resolves to the failure's sentence only when this screen had already
   *  unmounted by the time it came back — the relaunch after a sign-in the
   *  person started here and finished elsewhere (#146). The shell's strip
   *  shows it then (B9); on screen, the rail's own alert does. */
  launch: () => Promise<string | void>;
  preflighting: boolean;
  preflightResult: PreflightResult | null;
  preflightError: string | null;
  /** Same remount purpose as errorSeq, for the preflight alert. */
  preflightErrorSeq: number;
  /** Whether preflightResult/preflightError are graded from the request buildRunInput would send RIGHT NOW. */
  preflightIsCurrent: boolean;
  /** Current body AND graded less than PREFLIGHT_FRESH_MS ago. */
  preflightFresh: boolean;
  /** A fresh verdict for the current body that Launch would be refused on:
   *  a 4xx (not model_credential, not a 429) or a missing backend / llm_access
   *  row. 5xx, network errors and 429 never block. Disables Launch. */
  preflightBlock: boolean;
  /** The current body's last check was answered 429: nothing was graded. */
  preflightNotChecked: boolean;
  preflight: () => Promise<void>;
  /** The request Launch would send right now (null when the mode cannot
   *  represent the selections) — the identity a click-armed relaunch is held to. */
  currentBody: string | null;
  draftRevision: number;
  /** Preflight's own model-credential refusal, apart from launch's: the body
   *  it graded and the provider it names ("" when none). Its sign-in re-checks;
   *  it never launches. */
  preflightRefusal: PreflightRefusal | null;
}

export interface PreflightRefusal {
  body: string;
  provider: string;
}

// Preflight must grade the same body Launch would send, without owning a
// second copy of the form's state.
export function useLaunch({
  state,
  workspaces,
  modelProviders,
  policyMode,
  ccTouched,
  merged,
  onLaunchError,
  autoCheck,
  doorOpen,
  adoDoorOpen,
  identity,
  externalRevision,
  sourceRefreshPending,
  onCreated,
}: UseLaunchParams): UseLaunchResult {
  const navigate = useNavigate();
  const latestIdentity = React.useRef(identity);
  latestIdentity.current = identity;
  const mounted = React.useRef(true);
  React.useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
    };
  }, []);

  const [launching, setLaunching] = React.useState(false);
  const attempt = React.useRef(0);
  // Rulebook §7: disable Launch the instant it fires, but only show the
  // spinner once the request has been running long enough to need one.
  const { disabled: launchDisabled, showSpinner: launchSpinning } = useDeferredBusy(launching);
  const [error, setError] = React.useState<string | null>(null);
  const [errorSeq, setErrorSeq] = React.useState(0);
  const [errorPolicy, setErrorPolicy] = React.useState<PolicyRef | undefined>(undefined);
  const [credentialRefused, setCredentialRefused] = React.useState(false);
  const [refusedProvider, setRefusedProvider] = React.useState("");
  const authGeneration = getAuthGeneration();
  React.useEffect(() => {
    setError(null);
    setErrorPolicy(undefined);
    setCredentialRefused(false);
    setRefusedProvider("");
  }, [identity.principal, identity.revision, authGeneration]);

  const input = buildRunInput({ state, workspaces, modelProviders, policyMode, ccTouched, merged });
  const currentBody = input ? JSON.stringify(runWireBody(input)) : null;
  // Reference bodies may omit attachment details. Changing those selections
  // still invalidates the check, even if the serialized body stays identical.
  const selectionKey = JSON.stringify([policyMode, state.workspaces]);
  const draftKey = JSON.stringify([selectionKey, currentBody]);
  const draft = React.useRef({ key: draftKey, revision: 0 });
  if (draft.current.key !== draftKey) draft.current = { key: draftKey, revision: draft.current.revision + 1 };
  const draftRevision = draft.current.revision;
  const checks = useRunChecks({ input, body: currentBody, selectionKey, identity, externalRevision, sourceRefreshPending, autoCheck, doorOpen, adoDoorOpen });

  const launch = async () => {
    if (identity.principal !== latestIdentity.current.principal || !latestIdentity.current.resolved || draftRevision !== draft.current.revision) return;
    if (!input || !identity.resolved || !identity.principal || identity.authGeneration !== getAuthGeneration() || isSignedOutHold()) return;
    const request = ++attempt.current;
    const owner = identity.principal;
    const auth = getAuthGeneration();
    setError(null);
    setErrorPolicy(undefined);
    setCredentialRefused(false);
    setRefusedProvider("");
    setLaunching(true);
    try {
      const created: CreateRunResult = await runsApi.createRun(input);
      // #125: a launch that answers 2xx always navigates, in the same tick —
      // no held screen, no timer (a timer both raced every other way off this
      // screen — Esc and the ghost "Runs" button each land on /runs — and gave
      // a multi-line advisory a fixed beat nobody can finish reading). Any
      // advisory `warnings[]` ride along as router state for the run page to
      // render; they are NOT persisted (the durable record is the run.create
      // audit row's own clamp warnings), so they are gone the moment the
      // member reloads that page.
      if (latestIdentity.current.principal !== owner || !latestIdentity.current.resolved || latestIdentity.current.authGeneration !== getAuthGeneration()) return;
      onCreated();
      void navigate(`/runs/${encodeURIComponent(created.id)}`, { state: { launchWarnings: created.warnings ?? [] } });
    } catch (e) {
      if (latestIdentity.current.principal !== owner || auth !== getAuthGeneration()) return;
      const server = getErrorMessage(e);
      // B9 renders the SERVER's sentence verbatim: with none, the strip says
      // nothing rather than showing this screen's own fallback.
      if (!mounted.current) return server || undefined;
      const sentence = server || "Failed to launch run.";
      setError(sentence);
      setErrorSeq((n) => n + 1);
      setErrorPolicy(e instanceof HttpError ? e.policy : undefined);
      setCredentialRefused(isCredentialRefusal(e));
      setRefusedProvider(isCredentialRefusal(e) && e instanceof HttpError ? e.provider : "");
      onLaunchError?.(e);
    } finally {
      if (mounted.current && request === attempt.current) setLaunching(false);
    }
  };

  return {
    launching,
    launchDisabled,
    launchSpinning,
    error,
    errorSeq,
    errorPolicy,
    credentialRefused,
    refusedProvider,
    launch,
    ...checks,
    currentBody,
    draftRevision,
  };
}
