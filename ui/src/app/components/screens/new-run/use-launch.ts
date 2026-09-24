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
}

export interface UseLaunchResult {
  launching: boolean;
  launchDisabled: boolean;
  launchSpinning: boolean;
  error: string | null;
  /** Bumped on every failed launch, including a repeat of the same message —
   *  so the rail's alert region remounts and gets re-announced (#459). */
  errorSeq: number;
  credentialRefused: boolean;
  launchWarnings: string[];
  launchedRunId: string | null;
  launch: () => Promise<void>;
  preflighting: boolean;
  preflightResult: PreflightResult | null;
  preflightError: string | null;
  /** Same remount purpose as errorSeq, for the preflight alert. */
  preflightErrorSeq: number;
  /** Whether preflightResult/preflightError are graded from the request buildRunInput would send RIGHT NOW. */
  preflightIsCurrent: boolean;
  preflight: () => Promise<void>;
}

// New Run's launch + preflight state and actions — split out of
// new-run-screen.tsx (see that file's header). `state`/`workspaces`/`useSaved`/
// `ccTouched`/`merged` are the screen's own form state, read here rather than
// duplicated: buildRunInput composes the wire body from exactly what the form
// shows, so the screen and this hook can never author two different requests.
export function useLaunch({ state, workspaces, useSaved, ccTouched, merged, onLaunchError }: UseLaunchParams): UseLaunchResult {
  const navigate = useNavigate();

  const [launching, setLaunching] = React.useState(false);
  // Rulebook §7: disable Launch the instant it fires, but only show the
  // spinner once the request has been running long enough to need one.
  const { disabled: launchDisabled, showSpinner: launchSpinning } = useDeferredBusy(launching);
  const [error, setError] = React.useState<string | null>(null);
  const [errorSeq, setErrorSeq] = React.useState(0);
  const [credentialRefused, setCredentialRefused] = React.useState(false);
  // The 201's advisory `warnings[]` (§5c.8) — inline in the rail, not a toast.
  const [launchWarnings, setLaunchWarnings] = React.useState<string[]>([]);
  // Set ONLY while a launched run's advisories are on screen — the rail's
  // "Open run" is what carries the member there, at their own pace.
  const [launchedRunId, setLaunchedRunId] = React.useState<string | null>(null);
  // Preflight is a dry-run of the SAME request Launch sends — see buildRunInput
  // below. Independent loading/result/error state from Launch's: the two
  // actions can be in flight or have failed independently of one another.
  const [preflighting, setPreflighting] = React.useState(false);
  const [preflightResult, setPreflightResult] = React.useState<PreflightResult | null>(null);
  const [preflightError, setPreflightError] = React.useState<string | null>(null);
  const [preflightErrorSeq, setPreflightErrorSeq] = React.useState(0);
  // The request body the verdict on screen was graded FROM. A preflight result
  // is a statement about one body, and the rail renders it directly above
  // Launch as "the last thing read before committing" — so the moment the body
  // stops matching (policy document, confinement pick, workspace, drive, any
  // wizard field at all), the verdict stops being about the run that is about
  // to launch and must not be shown. Held as state, not a ref, so an edit made
  // WHILE a preflight is in flight also invalidates the answer when it lands.
  const [preflightedBody, setPreflightedBody] = React.useState<string | null>(null);

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
    setLaunching(true);
    setLaunchWarnings([]);
    setLaunchedRunId(null);
    try {
      const created: CreateRunResult = await runsApi.createRun(buildRunInput());
      const warnings = created.warnings ?? [];
      // §5c.8: a run that launched WITH advisories is never navigated away from
      // on a clock. A 1.6s timer both raced every other way off this screen
      // (Esc and the ghost "Runs" button each landed on /runs, then the timer
      // yanked the member to /runs/:id) and gave a multi-line advisory a fixed
      // beat nobody can finish reading. The screen HOLDS instead: the warnings
      // stay listed in the rail and Launch becomes OPEN_RUN_CTA, which is the
      // only thing that navigates. No timer.
      if (warnings.length > 0) {
        setLaunchWarnings(warnings);
        setLaunchedRunId(created.id);
        setLaunching(false); // nothing reads it once onOpenRun is set.
      } else {
        navigate(`/runs/${encodeURIComponent(created.id)}`);
      }
    } catch (e) {
      setError(getErrorMessage(e) || "Failed to launch run.");
      setErrorSeq((n) => n + 1);
      setCredentialRefused(isCredentialRefusal(e));
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

  const preflight = async () => {
    // The saved lane with nothing picked has NO body to dry-run — falling
    // through would preflight the leftover Custom document this lane will
    // never launch, breaking buildRunInput's same-body invariant. (The panel
    // disables the button in this state too; this guards the race.)
    if (useSaved && !state.selectedPolicyId) return;
    setPreflightError(null);
    setPreflightResult(null);
    setPreflightedBody(null);
    setPreflighting(true);
    // Grade the body we actually send, and remember exactly that one.
    const body = buildRunInput();
    const key = JSON.stringify(body);
    try {
      setPreflightResult(await runsApi.preflightRun(body));
    } catch (e) {
      setPreflightError(getErrorMessage(e) || "Preflight failed.");
      setPreflightErrorSeq((n) => n + 1);
    } finally {
      setPreflightedBody(key);
      setPreflighting(false);
    }
  };

  return {
    launching,
    launchDisabled,
    launchSpinning,
    error,
    errorSeq,
    credentialRefused,
    launchWarnings,
    launchedRunId,
    launch,
    preflighting,
    preflightResult,
    preflightError,
    preflightErrorSeq,
    preflightIsCurrent,
    preflight,
  };
}
