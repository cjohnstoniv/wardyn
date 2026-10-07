/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { useNavigate } from "react-router-dom";
import type { CreateRunResult, PreflightResult } from "../../../lib/types";
import { isCredentialRefusal, runs as runsApi } from "../../../lib/api/runs";
import { HttpError, isSignedOutHold } from "../../../lib/api/core";
import type { PolicyRef } from "../../../lib/api/health";
import { useDeferredBusy } from "../../../lib/use-deferred-busy";
import { getErrorMessage } from "../../../lib/format";
import { buildRunInput, type RunInputParams } from "./build-run-input";

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
}

/** A refusal this fresh still describes the body. Past it, Launch is the
 *  server's decision again. */
export const PREFLIGHT_FRESH_MS = 60_000;
/** Settle time before a body is checked on its own. */
export const PREFLIGHT_DEBOUNCE_MS = 800;
// A refusal of these classes (and any 401) is repaired by something other than
// an edit (a sign-in, a retry), so it is shown but never holds Launch.
const NEVER_BLOCKS = new Set(["model_credential", "preflight_rate_limited"]);

export interface UseLaunchResult {
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
}: UseLaunchParams): UseLaunchResult {
  const navigate = useNavigate();
  const mounted = React.useRef(true);
  React.useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
    };
  }, []);

  const [launching, setLaunching] = React.useState(false);
  // Rulebook §7: disable Launch the instant it fires, but only show the
  // spinner once the request has been running long enough to need one.
  const { disabled: launchDisabled, showSpinner: launchSpinning } = useDeferredBusy(launching);
  const [error, setError] = React.useState<string | null>(null);
  const [errorSeq, setErrorSeq] = React.useState(0);
  const [errorPolicy, setErrorPolicy] = React.useState<PolicyRef | undefined>(undefined);
  const [credentialRefused, setCredentialRefused] = React.useState(false);
  const [refusedProvider, setRefusedProvider] = React.useState("");
  // Preflight is a dry-run of the SAME request Launch sends. Independent
  // loading/result/error state from Launch's: the two
  // actions can be in flight or have failed independently of one another.
  const [preflighting, setPreflighting] = React.useState(false);
  const [preflightResult, setPreflightResult] = React.useState<PreflightResult | null>(null);
  const [preflightError, setPreflightError] = React.useState<string | null>(null);
  const [preflightErrorSeq, setPreflightErrorSeq] = React.useState(0);
  const [preflightRefusal, setPreflightRefusal] = React.useState<PreflightRefusal | null>(null);
  // The request body the verdict on screen was graded FROM. A preflight result
  // is a statement about one body, and the rail renders it directly above
  // Launch as "the last thing read before committing" — so the moment the body
  // stops matching (policy document, confinement pick, workspace, drive, any
  // wizard field at all), the verdict stops being about the run that is about
  // to launch and must not be shown. Held as state, not a ref, so an edit made
  // WHILE a preflight is in flight also invalidates the answer when it lands.
  // `at` and `status` are what the freshness and block rules read: a verdict is
  // a 4xx only when `status` says so (2xx = 200, no answer = 0).
  const [graded, setGraded] = React.useState<{ body: string; requestKey: string; at: number; status: number; reason: string } | null>(null);
  const preflightedBody = graded?.body ?? null;
  // Forces the render that lets a verdict age out; the clock itself is read in render.
  const [, setAgeTick] = React.useState(0);
  const abortRef = React.useRef<AbortController | null>(null);
  const seqRef = React.useRef(0);
  const inFlightRef = React.useRef(false);
  const debounceRef = React.useRef<ReturnType<typeof setTimeout> | null>(null);

  const input = buildRunInput({ state, workspaces, modelProviders, policyMode, ccTouched, merged });
  const currentBody = input ? JSON.stringify(input) : null;
  // Reference bodies may omit attachment details. Changing those selections
  // still invalidates the check, even if the serialized body stays identical.
  const requestKey = JSON.stringify([policyMode, state.workspaces, currentBody]);

  const launch = async () => {
    if (!input) return;
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
      void navigate(`/runs/${encodeURIComponent(created.id)}`, { state: { launchWarnings: created.warnings ?? [] } });
    } catch (e) {
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
      setLaunching(false);
    }
  };

  // A changed body or attachment must hide the verdict on the first render,
  // before the invalidation effect clears the previous check.
  const preflightIsCurrent = preflightedBody !== null && preflightedBody === currentBody && graded?.requestKey === requestKey;
  const preflightFresh = preflightIsCurrent && !!graded && Date.now() - graded.at < PREFLIGHT_FRESH_MS;
  const preflightNotChecked = preflightIsCurrent && graded?.status === 429;
  React.useEffect(() => {
    if (!graded) return;
    const t = setTimeout(() => setAgeTick((n) => n + 1), Math.max(0, graded.at + PREFLIGHT_FRESH_MS - Date.now()) + 1);
    return () => clearTimeout(t);
  }, [graded]);

  const preflight = async () => {
    if (!input || currentBody === null) return;
    // One check at a time: a newer one supersedes the older, whose answer is
    // then never applied. The previous verdict stays on screen until the new
    // one lands, so a re-check of the same body never un-blocks Launch.
    abortRef.current?.abort();
    const ctl = new AbortController();
    abortRef.current = ctl;
    const seq = ++seqRef.current;
    inFlightRef.current = true;
    setPreflighting(true);
    // Grade the body we actually send, and remember exactly that one.
    const key = currentBody;
    try {
      const res = await runsApi.preflightRun(input, ctl.signal);
      if (ctl.signal.aborted || seqRef.current !== seq) return;
      setPreflightResult(res);
      setPreflightError(null);
      setPreflightRefusal(null);
      setGraded({ body: key, requestKey, at: Date.now(), status: 200, reason: "" });
    } catch (e) {
      if (ctl.signal.aborted || seqRef.current !== seq) return;
      const status = e instanceof HttpError ? e.status : 0;
      setPreflightResult(null);
      setPreflightRefusal(null);
      // A 429 means "not checked": no alert, no block, no automatic retry.
      if (status === 429) {
        setPreflightError(null);
      } else {
        // The alert already speaks RAIL.PREFLIGHT_ERROR_LABEL first; a fallback
        // that repeats it read "Preflight failed Preflight failed." (#497).
        setPreflightError(getErrorMessage(e) || "No reason was given.");
        setPreflightErrorSeq((n) => n + 1);
        if (isCredentialRefusal(e)) setPreflightRefusal({ body: key, provider: e instanceof HttpError ? e.provider : "" });
      }
      setGraded({ body: key, requestKey, at: Date.now(), status, reason: e instanceof HttpError ? e.reason : "" });
    } finally {
      if (seqRef.current === seq) {
        inFlightRef.current = false;
        setPreflighting(false);
      }
    }
  };

  // Launch is held on a fresh verdict for THIS body that the server refused,
  // or whose rows say Launch would be refused (f-f4 backend, f-f5 llm_access).
  const answeredRefusal =
    !!graded && graded.status >= 400 && graded.status < 500 && graded.status !== 401 && graded.status !== 429 && !NEVER_BLOCKS.has(graded.reason);
  const rowMissing = (kind: string) =>
    !!preflightResult?.setup_items?.some((i) => i.kind === kind && i.status === "missing");
  const preflightBlock =
    preflightFresh &&
    (answeredRefusal || (autoCheck.backendArm && rowMissing("backend")) || (autoCheck.modelArm && rowMissing("llm_access")));

  // Automatic preflight. Everything below goes through ONE debounce, and the
  // gate is read when the timer fires (through refs), so a check never
  // outlives the state that allowed it. A signed-out hold skips it: a
  // background POST must never raise the sign-in prompt by itself.
  const preflightRef = React.useRef(preflight);
  preflightRef.current = preflight;
  const localRef = React.useRef(autoCheck.local);
  localRef.current = autoCheck.local;
  const scheduleCheck = React.useCallback(() => {
    if (debounceRef.current) clearTimeout(debounceRef.current);
    debounceRef.current = setTimeout(() => {
      debounceRef.current = null;
      if (!mounted.current || inFlightRef.current || !localRef.current || isSignedOutHold()) return;
      void preflightRef.current();
    }, PREFLIGHT_DEBOUNCE_MS);
  }, []);
  // Never resurrect a completed verdict after switching away and back. An
  // aborted transport may settle late, so release its busy state immediately.
  React.useEffect(() => {
    setGraded(null);
    setPreflightResult(null);
    setPreflightError(null);
    setPreflightRefusal(null);
    setPreflighting(false);
    if (currentBody !== null) scheduleCheck();
    return () => {
      if (debounceRef.current) clearTimeout(debounceRef.current);
      debounceRef.current = null;
      abortRef.current?.abort();
      seqRef.current = seqRef.current + 1;
      inFlightRef.current = false;
    };
  }, [currentBody, requestKey, scheduleCheck]);
  // The form becoming checkable without the body changing (a late provider list).
  React.useEffect(() => {
    if (autoCheck.local) scheduleCheck();
  }, [autoCheck.local, scheduleCheck]);
  // Coming back to the tab, or the sign-in door closing, re-checks the body.
  React.useEffect(() => {
    window.addEventListener("focus", scheduleCheck);
    return () => window.removeEventListener("focus", scheduleCheck);
  }, [scheduleCheck]);
  const doorWasOpen = React.useRef(doorOpen);
  React.useEffect(() => {
    if (doorWasOpen.current && !doorOpen) scheduleCheck();
    doorWasOpen.current = doorOpen;
  }, [doorOpen, scheduleCheck]);

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
    preflighting,
    preflightResult,
    preflightError,
    preflightErrorSeq,
    preflightIsCurrent,
    preflightFresh,
    preflightBlock,
    preflightNotChecked,
    preflight,
    currentBody,
    preflightRefusal,
  };
}
