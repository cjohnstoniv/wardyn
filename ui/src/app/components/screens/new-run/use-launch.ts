/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New Run's launch + preflight lane — split out because new-run-screen.tsx sits
// at the file-size gate's ceiling (scripts/check-file-size.sh, 1000 lines):
// buildRunInput, launch and preflight move here verbatim, as a hook the screen
// calls, rather than as pure functions (policy-lane.ts's pattern) — this lane
// owns React state (in-flight flags, the last result, the last graded body),
// not just a derivation over state the screen already holds.
import * as React from "react";
import { useNavigate } from "react-router-dom";
import type { CreateRunResult, PreflightResult } from "../../../lib/types";
import { isCredentialRefusal, runs as runsApi } from "../../../lib/api/runs";
import { HttpError, isSignedOutHold } from "../../../lib/api/core";
import { useDeferredBusy } from "../../../lib/use-deferred-busy";
import { getErrorMessage } from "../../../lib/format";
import { primaryWorkspaceId, type WizardState } from "./wizard-types";
import { buildSpec, mergeRunSelections } from "./wizard-spec";
import type { Workspace } from "../../../lib/types";

export interface UseLaunchParams {
  state: WizardState;
  workspaces: Workspace[];
  /** The mode row: launch by reference (a saved policy) vs. an authored document. */
  useSaved: boolean;
  /** Whether the Barrier control carries an EXPLICIT pick — see new-run-screen.tsx's ccTouched. */
  ccTouched: boolean;
  /** The post-parse union of the authored spec with this run's own selections, or null while the spec doesn't parse. */
  merged: ReturnType<typeof mergeRunSelections> | null;
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
// A refusal of these classes is repaired by something other than an edit (a
// sign-in, a retry), so it is shown but never holds Launch.
const NEVER_BLOCKS = new Set(["model_credential", "preflight_rate_limited"]);

export interface UseLaunchResult {
  launching: boolean;
  launchDisabled: boolean;
  launchSpinning: boolean;
  error: string | null;
  /** Bumped on every failed launch, including a repeat of the same message —
   *  so the rail's alert region remounts and gets re-announced (#459). */
  errorSeq: number;
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
  preflight: () => Promise<void>;
  /** The request Launch would send right now (null while the policy document
   *  is unparseable) — the identity a click-armed relaunch is held to. */
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

// New Run's launch + preflight state and actions — split out of
// new-run-screen.tsx (see that file's header). `state`/`workspaces`/`useSaved`/
// `ccTouched`/`merged` are the screen's own form state, read here rather than
// duplicated: buildRunInput composes the wire body from exactly what the form
// shows, so the screen and this hook can never author two different requests.
export function useLaunch({ state, workspaces, useSaved, ccTouched, merged, onLaunchError, autoCheck, doorOpen }: UseLaunchParams): UseLaunchResult {
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
  const [credentialRefused, setCredentialRefused] = React.useState(false);
  const [refusedProvider, setRefusedProvider] = React.useState("");
  // Preflight is a dry-run of the SAME request Launch sends — see buildRunInput
  // below. Independent loading/result/error state from Launch's: the two
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
  const [graded, setGraded] = React.useState<{ body: string; at: number; status: number; reason: string } | null>(null);
  const preflightedBody = graded?.body ?? null;
  // Forces the render that lets a verdict age out; the clock itself is read in render.
  const [, setAgeTick] = React.useState(0);
  const abortRef = React.useRef<AbortController | null>(null);
  const seqRef = React.useRef(0);
  const inFlightRef = React.useRef(false);
  const debounceRef = React.useRef<ReturnType<typeof setTimeout> | null>(null);

  // The ONE request-payload builder — Launch and Preflight must send EXACTLY
  // the same body, since preflight's verdict is only true if it is a dry-run
  // of what Launch actually does. A second builder here is how the two drift.
  const buildRunInput = () => {
    const { run: built } = buildSpec(state, workspaces);
    // Untouched Barrier control (ccTouched): OMIT confinement_class so the
    // server's own default decides and its audit trail reads `defaulted`.
    const run = ccTouched ? built : { ...built, confinement_class: undefined };
    // The MODE ROW is the discriminator: a policy id that somehow survives a
    // switch back to Custom still must not launch by reference. And the
    // workspace_id override must never OVERWRITE buildSpec's deliberate
    // ephemeral-workspace fallback with undefined — that silently launched a
    // workspace-less run.
    if (useSaved && state.selectedPolicyId) {
      return {
        ...run,
        policy_id: state.selectedPolicyId,
        workspace_id: primaryWorkspaceId(state.workspaces, workspaces) ?? run.workspace_id,
      };
    }
    // Unreachable: `problem` disables both actions while the document is
    // broken. Throwing beats substituting a composed fallback nobody wrote.
    if (!merged) throw new Error("The policy spec isn't valid JSON.");
    return { ...run, inline_policy: merged.spec };
  };

  const launch = async () => {
    setError(null);
    setCredentialRefused(false);
    setRefusedProvider("");
    setLaunching(true);
    try {
      const created: CreateRunResult = await runsApi.createRun(buildRunInput());
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
      setCredentialRefused(isCredentialRefusal(e));
      setRefusedProvider(isCredentialRefusal(e) && e instanceof HttpError ? e.provider : "");
      onLaunchError?.(e);
      setLaunching(false);
    }
  };

  // A dry-run of launch's own resolution: same body, same 4xx surface, but
  // mints/dispatches nothing. Renders the member-clamp warnings, the risk
  // grade, and the confinement class the run will actually be enforced at.
  // The identity of the request Launch would send right now. buildRunInput
  // throws while the policy document is unparseable (`problem` disables both
  // actions in that state), which is itself a body change — hence the catch.
  const currentBody = (() => {
    try {
      return JSON.stringify(buildRunInput());
    } catch {
      return null;
    }
  })();
  // Stale BY CONSTRUCTION rather than by operator discipline: nothing has to
  // remember to clear the verdict, because a verdict graded from a different
  // body is never rendered in the first place.
  const preflightIsCurrent = preflightedBody !== null && preflightedBody === currentBody;
  const preflightFresh = preflightIsCurrent && !!graded && Date.now() - graded.at < PREFLIGHT_FRESH_MS;
  React.useEffect(() => {
    if (!graded) return;
    const t = setTimeout(() => setAgeTick((n) => n + 1), Math.max(0, graded.at + PREFLIGHT_FRESH_MS - Date.now()) + 1);
    return () => clearTimeout(t);
  }, [graded]);

  const preflight = async () => {
    // The saved lane with nothing picked has NO body to dry-run — falling
    // through would preflight the leftover Custom document this lane will
    // never launch, breaking buildRunInput's same-body invariant. (The panel
    // disables the button in this state too; this guards the race.)
    if (useSaved && !state.selectedPolicyId) return;
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
    const body = buildRunInput();
    const key = JSON.stringify(body);
    try {
      const res = await runsApi.preflightRun(body, ctl.signal);
      if (ctl.signal.aborted) return;
      setPreflightResult(res);
      setPreflightError(null);
      setPreflightRefusal(null);
      setGraded({ body: key, at: Date.now(), status: 200, reason: "" });
    } catch (e) {
      if (ctl.signal.aborted) return;
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
      setGraded({ body: key, at: Date.now(), status, reason: e instanceof HttpError ? e.reason : "" });
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
    !!graded && graded.status >= 400 && graded.status < 500 && graded.status !== 429 && !NEVER_BLOCKS.has(graded.reason);
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
  // A body change restarts the debounce and aborts whatever was grading the old one.
  React.useEffect(() => {
    if (currentBody === null) return;
    scheduleCheck();
    return () => {
      if (debounceRef.current) clearTimeout(debounceRef.current);
      debounceRef.current = null;
      abortRef.current?.abort();
    };
  }, [currentBody, scheduleCheck]);
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
    preflight,
    currentBody,
    preflightRefusal,
  };
}
