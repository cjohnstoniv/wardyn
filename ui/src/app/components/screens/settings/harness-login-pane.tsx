/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// HarnessLoginPane — "Connect via container login" for a Claude subscription in
// deployments with no host ~/.claude (compose/team). It launches an interactive
// login sandbox IMMEDIATELY (opening the pane IS the intent — no extra button),
// embeds the AttachTerminal, AUTO-TYPES `claude setup-token`, AUTO-OPENS the
// printed OAuth URL in a new browser tab, and AUTO-CAPTURES the printed
// long-lived token straight off the terminal stream — then stores it and injects
// it proxy-side into every later run; the sandbox never holds a live credential.
// The only unavoidable human step is approving the OAuth in the browser and
// pasting the callback code back into the terminal. A manual paste field remains
// as a fallback if auto-capture misses. Renders inline (never routes away).
//
// The AWS flow adds one step BEFORE the sandbox launches: it asks for the
// organization's access portal (start) URL, because `aws sso login` reads
// sso_start_url + sso_region from ~/.aws/config and Wardyn stores no start URL
// (the region is daemon boot config). The server seeds both into the sandbox as
// a credential-free ~/.aws/config, which is what makes the auto-typed
// `aws sso login --sso-session wardyn …` run unattended.
import * as React from "react";
import { Loader2, ShieldCheck, TriangleAlert, KeyRound, Square, ExternalLink, CornerDownLeft } from "lucide-react";
import { HttpError } from "../../../lib/api/core";
import { harnessAuth as harnessAuthApi } from "../../../lib/api/harness-auth";
import { runs as runsApi } from "../../../lib/api/runs";
import { isTerminalRunState, type AgentRun } from "../../../lib/types";
import { usePoll } from "../../../lib/use-poll";
import { AGENTS } from "../../../lib/workspace-providers-copy";
import { AttachTerminal, type AttachTerminalHandle } from "../../attach-terminal";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";
import {
  LOGIN_SANDBOX_READ_RETRYING,
  LOGIN_SANDBOX_SLOW_START,
  LOGIN_SANDBOX_STUCK_LEAD_IN,
  startWaitVerdict,
  type StartWaitVerdict,
} from "./login-start-wait";
import { isTerminalStatusReason, statusDetailSentence } from "../run-status-detail";
import {
  CAPTURE_CHECK_UNREACHABLE,
  CAPTURE_HANDOFF,
  CAPTURE_NOT_CORROBORATED,
  CAPTURE_VERIFYING,
  confirmCaptureWithServer,
  extractSignedIn,
  watchForCapture,
} from "./capture-confirm";

// RE-EXPORTED, not re-declared: the corroboration rule and its refusal
// sentence moved to capture-confirm.ts to keep this file under the size cap, and
// every existing importer — this pane's tests, ui/e2e — keeps its import path.
export { CAPTURE_NOT_CORROBORATED, serverConfirmsCapture } from "./capture-confirm";
// Finding 7a: the verification tab's open/navigate/close lifecycle.
import { AUTH_TAB_BLOCKED_NOTE, openAuthTab, type AuthTab } from "./auth-tab-handle";
import {
  LOGIN_SANDBOX_STARTING,
  LOGIN_SANDBOX_UNREADABLE,
  SELFRUN_MARKER,
} from "./login-pane-copy";
export { LOGIN_SANDBOX_STARTING, LOGIN_SANDBOX_UNREADABLE, SELFRUN_MARKER } from "./login-pane-copy";
// review-1 S4: the per-provider flow table is pure data + one presentational
// component (ExpectList) — EXTRACTED to login-flows.tsx to keep this file
// under the size cap. Re-exported so agents-tab.tsx and this pane's own
// pinned tests keep their import path.
import { ExpectList, loginFlow } from "./login-flows";
export { loginFlow, LOGIN_FLOWS } from "./login-flows";
export type { CaptureMode, LoginFlow } from "./login-flows";
// review-1 S4: the raw-PTY extractors are pure functions — no React, no pane
// state — EXTRACTED to login-pty-extract.ts to keep this file under the size
// cap. Re-exported so this pane's own pinned tests keep their import path.
import {
  SANDBOX_REFUSAL_LEAD_IN,
  extractAuthUrl,
  extractDeviceVerificationUrl,
  extractFailSentence,
  extractSetupToken,
} from "./login-pty-extract";
export { SANDBOX_REFUSAL_LEAD_IN, extractAuthUrl, extractFailSentence, extractSetupToken } from "./login-pty-extract";

// "intro" is the consent gate: nothing launches until the operator has read
// what is about to happen and clicked Start — skipping it would fire the pane
// on mount instead: a dialog, then suddenly a terminal, then suddenly a
// browser auth prompt, with nothing saying what was coming or what would be
// asked of you.
type Phase = "intro" | "prompt" | "launching" | "starting" | "attached" | "saving" | "done" | "error";

// How often the pane asks whether the login sandbox is up yet. Same cadence
// demo-runner's own launch→starting→live machine uses, and for the same reason:
// it is a human watching a pull, not a control loop.
const RUN_POLL_MS = 2000;

// What ENDS the wait, and what it says while it lasts, is a clock now — see
// login-start-wait.ts. The tick budget this replaces called itself "≈30s" and
// could mean anything from 30 seconds of fast 5xx to fifteen minutes of hung
// reads, while a measured 131-second cold pull with healthy reads never tripped
// it at all (finding 6).

// LOGIN_SANDBOX_STARTING lives in ./login-pane-copy with the other two strings
// something outside the browser bundle reads (U-15: ui/e2e/providers.spec.ts
// asserts through it, and a Playwright spec cannot import THIS module).
// DRAFT (M2 canon pending) — the same wait ending badly on a run that carries
// no failure_hint of its own (a kill, a stop). Says only what is known: the
// sandbox is gone and nothing was captured.
const LOGIN_SANDBOX_ENDED = "The sign-in sandbox stopped before it was ready — nothing was captured. Try again.";
// LOGIN_SANDBOX_UNREADABLE lives in ./login-pane-copy (imported and re-exported
// above): the live walk asserts through it, and a Playwright spec cannot import
// THIS module — it reaches AttachTerminal's xterm.css, which Node cannot load.

// DRAFT (M2 canon pending) — the sandbox signs itself in now. The aws-sso image
// starts the chained command in its own tmux session before its prep
// (deploy/images/aws-sso/agent-run → signin-pane.sh), and every attach path —
// this pane, the Runs list, `wardyn attach`, ssh — joins that one session. So the
// pane no longer hands AttachTerminal an `autoRun` for aws: that unconditional
// type would land on the running login's stdin and run a SECOND wardyn-aws-sso in
// the same run, which the server refuses as already_captured — a fail marker on a
// sign-in that worked.
//
// It cannot simply stop typing either. The aws-sso tag is version-locked on the
// ghcr default, but an operator WARDYN_AGENT_IMAGES pin (what private estates use)
// makes a console-N+1 / image-N pairing real, and on an image-N sandbox nothing
// types the pair at all — finding 4 again. So: wait out a grace window and type
// ONLY if the sandbox has not announced itself. The image prints this marker as
// its FIRST act, before its own prep wait, precisely so it beats this timer; the
// grace is long enough for a slow first paint and far shorter than the device
// code's ~600 s life.
//
// The timer lives HERE, never in AttachTerminal: the Runs-list mount
// (run-detail/terminal-notice.tsx) shares that component, and a terminal that
// types on its own is how a read-only viewer would start a second sign-in.
// SELFRUN_MARKER itself is declared in ./login-pane-copy for the same reason.
const SELFRUN_GRACE_MS = 12_000;

// isLikelyStartUrl mirrors the server's validateSSOStartURL (harnesscred.go) so
// the operator sees the problem before a round trip. Deliberately loose — the
// server is the authority, and the egress policy, not this check, decides what
// the sandbox may dial. Exported for tests.
export function isLikelyStartUrl(s: string): boolean {
  const v = s.trim();
  return /^https:\/\/[^\s/]+/.test(v);
}

// Force the login terminal wide so `claude setup-token` never hard-wraps the
// OAuth URL (~250 chars) or the token across lines — a narrow PTY wrap mid-URL
// dropped response_type=code and produced "Invalid OAuth Request" on the opened
// tab. AttachTerminal pins BOTH the PTY and the visual xterm grid to this width
// (the pane scrolls horizontally): the CLI is a full-screen TUI that
// cursor-addresses whatever grid it is told, so a decoupled wide-PTY/narrow-view
// split interleaved its redraw frames into garbage on screen. Wrap-proofing the
// extractors instead is a dead end — rejoining a wrapped URL cannot tell where
// the URL ends and the next word begins, and a fused tail corrupts &state=.
const LOGIN_PTY_COLS = 512;

/** What a PARENT can make this pane do. One verb: the pane owns the login run's
 *  id, so it is the only thing that can end that run — and a dialog's Escape /
 *  overlay click closes the PARENT, which `onCancel` (child-to-parent) cannot
 *  reach back into. Without this a dismissed dialog left a "wardyn: sign-in
 *  running" run on the member's board for up to 30 minutes. */
export interface HarnessLoginPaneHandle {
  cancel: () => void;
}

export function HarnessLoginPane({
  provider = "anthropic",
  startURLManaged = false,
  onDone,
  onCancel,
  paneRef,
}: {
  provider?: string;
  // The ORG's access portal is already stored and the server will use it: this
  // sign-in runs under a per_user agent row (the member's Getting Started CTA,
  // and the admin's own sign-in on a per_user row of the Agents tab). The
  // start-URL PROMPT is then skipped for a one-line note — harnessLogin's
  // startUrl argument is ignored server-side under such a row, so asking was a
  // field whose value could not take effect, and every member had to hunt down a
  // URL their admin had already entered.
  //
  // Default false: the ordinary Settings flow (no row, or `shared`) has nothing
  // stored to sign in against, so it still asks.
  startURLManaged?: boolean;
  // Called after the token is captured (parent refreshes setup status + closes).
  onDone: () => void;
  // Called when the operator backs out before capturing.
  onCancel: () => void;
  // Optional: a parent that can be dismissed from OUTSIDE the pane (the
  // model-access door's dialog) holds this to route that dismissal through the
  // pane's own cancellation. Omitted everywhere the pane's own Cancel is the
  // only way out.
  paneRef?: React.Ref<HarnessLoginPaneHandle>;
}) {
  const flow = loginFlow(provider);
  const askStartUrl = !!flow.needsStartUrl && !startURLManaged;
  // review-1 B1: every mount site passes an INLINE `onDone` — a fresh function
  // identity on every parent re-render. `completeCapture` must not list
  // `onDone` in its own deps: that puts a fresh `completeCapture` in the watch
  // effect's deps, which tears the watch down and restarts it (a fresh 45-min
  // deadline, a fresh 5-min grace, the back-off reset to 2s) on every
  // unrelated parent render — 63 audit reads in 5 minutes, probed. Same ref
  // pattern attach-terminal.tsx:237-240 already uses for `onOutput`/`autoRun`.
  const onDoneRef = React.useRef(onDone);
  React.useEffect(() => {
    onDoneRef.current = onDone;
  }, [onDone]);
  const [phase, setPhase] = React.useState<Phase>(askStartUrl ? "prompt" : "intro");
  const [startUrl, setStartUrl] = React.useState("");
  const [runId, setRunId] = React.useState<string | null>(null);
  const [token, setToken] = React.useState("");
  const [error, setError] = React.useState("");
  const [autoCaptured, setAutoCaptured] = React.useState(false);
  // Whether the terminal was ever mounted on this run. The error phase keeps a
  // helper flow's terminal on screen for its SCROLLBACK (a refused capture's
  // only other artifact) — but a run that failed while still coming up has no
  // scrollback, and mounting AttachTerminal on it is the dead panel P5 is about:
  // the ticket mint 409s and one failed mint is terminal in that component.
  const [everAttached, setEverAttached] = React.useState(false);
  // U-11: whether the LAST launch was refused with a 409 rather than failing.
  // Reset on every launch attempt (see `launch`), so a refusal cannot outlive
  // the condition that caused it.
  const [refused, setRefused] = React.useState(false);
  // 0.7.6 finding 6: whether the SUBSTRATE gave a terminal answer. Suppresses
  // "Try again" for the reason `refused` does — the next attempt earns the same
  // answer — but a separate flag, because who can fix it differs. Reset per launch.
  const [stuck, setStuck] = React.useState(false);
  const [authUrl, setAuthUrl] = React.useState("");
  const [code, setCode] = React.useState("");
  // Finding 7a: the browser blocked even the click-backed tab — 0.7.5's link is the fallback.
  const [tabBlocked, setTabBlocked] = React.useState(false);
  // Finding 7b: the CLI's own success line — a HINT (S-13's own reasoning), never trusted alone.
  const [signedIn, setSignedIn] = React.useState(false);

  const termRef = React.useRef<AttachTerminalHandle>(null);
  // Opened on the click, navigated once the URL is known, closed on every exit path.
  const authTabRef = React.useRef<AuthTab | null>(null);
  const closeAuthTab = React.useCallback(() => {
    authTabRef.current?.close();
    authTabRef.current = null;
  }, []);
  // Aborts the background capture watch (Finding 7b) on unmount/relaunch/cancel.
  const watchAbortRef = React.useRef<AbortController | null>(null);
  // review-1 S2: the watch's "immediate tick" wake channel — a `new
  // Event("tick")` dispatched here cuts its current back-off wait short. One
  // instance for the pane's life; the watch effect passes it straight through.
  const watchWakeRef = React.useRef<EventTarget>(new EventTarget());

  // Rolling buffer of recent PTY output + latches so we act on each thing once.
  const outBufRef = React.useRef("");
  const savedRef = React.useRef(false);
  // The launch-after-dismiss race: harnessLogin's POST answers with the run id
  // after dispatch has already created the run, so a cancellation that lands
  // while it is in flight sees `runId === null` and kills nothing — the run is
  // born orphaned. Latched here on the way out and re-checked once the id
  // exists.
  const dismissedRef = React.useRef(false);
  const failedRef = React.useRef(false);
  const openedUrlRef = React.useRef(false);
  const signedInRef = React.useRef(false);
  // Once-only completion (Codex #9): guards completeCapture below.
  const completedRef = React.useRef(false);
  // confirmCapture kills the run BEFORE the corroboration round trip
  // (R-7) and, on success, calls completeCapture — which would otherwise kill
  // it a second time. One guard, shared by both.
  const killedRef = React.useRef(false);
  // review-1 S1: which sentence the SHORT round trip would have shown, so the
  // background watch's eventual refusal (if it gives up) says the same thing
  // — "" until a marker's own corroboration actually disagrees.
  const verifyFailSentenceRef = React.useRef("");
  // Consecutive unreadable polls of the starting run, not a total: one blip must
  // not end a sign-in that is working. A ref, not state — it drives no render
  // and must not churn the poll callback's identity. Same for the two clocks
  // beside it: when THIS launch began, and when the current run of failed reads
  // did (null while reads are healthy) — startWaitVerdict's whole input.
  const pollFailuresRef = React.useRef(0);
  const startedAtRef = React.useRef(0);
  const failingSinceRef = React.useRef<number | null>(null);
  // The graded wait, and the last grade RENDERED — so a tick that changes
  // nothing does not re-render the pane every two seconds.
  const [waitNote, setWaitNote] = React.useState<StartWaitVerdict>("starting");
  const waitNoteRef = React.useRef<StartWaitVerdict>("starting");
  // The substrate's sentence for the CURRENT wait, or "". It REPLACES the hedged
  // slow-start line rather than joining it: one wait, one sentence.
  const [startingSentence, setStartingSentence] = React.useState("");
  // ONE self-run grace timer per launch, armed on the FIRST attach.
  const selfRunArmedRef = React.useRef(false);

  const launch = React.useCallback(async () => {
    // Finding 7a: this must stay first, before any `await` below — the click
    // is the only user gesture this flow ever gets, and a popup blocker only
    // allows a tab while the call stack is inside that gesture. Hoisting an
    // `await` above this line silently reverts the lane.
    authTabRef.current?.close();
    const tab = openAuthTab();
    authTabRef.current = tab;
    setTabBlocked(!tab);
    watchAbortRef.current?.abort();
    watchAbortRef.current = null;
    completedRef.current = false;
    killedRef.current = false;
    verifyFailSentenceRef.current = "";
    signedInRef.current = false;
    setSignedIn(false);
    setPhase("launching");
    setError("");
    outBufRef.current = "";
    savedRef.current = false;
    dismissedRef.current = false;
    failedRef.current = false;
    openedUrlRef.current = false;
    setAutoCaptured(false);
    setEverAttached(false);
    pollFailuresRef.current = 0;
    startedAtRef.current = Date.now();
    failingSinceRef.current = null;
    waitNoteRef.current = "starting";
    setWaitNote("starting");
    setStartingSentence("");
    selfRunArmedRef.current = false;
    setRefused(false);
    setStuck(false);
    setAuthUrl("");
    try {
      const id = await harnessAuthApi.harnessLogin(provider, startUrl.trim());
      // The id comes first, the terminal later. Holding the id from t≈0 is what
      // makes Cancel able to kill a sandbox that is still coming up — P5 fixed
      // the POST not answering until dispatch was done, which let a timed-out
      // launch leave an orphan nobody could name.
      // …and if the way out was taken while that POST was in flight, this id is
      // the only handle anybody will ever have on the run it created.
      if (dismissedRef.current) {
        runsApi.killRun(id).catch(() => {});
        return;
      }
      setRunId(id);
      setPhase("starting");
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
      // U-11: a 409 is a REFUSAL, not a failure — the server declined this launch
      // for a reason still true a second later (a sign-in already running for
      // this principal, or the no-credential member preview,
      // harnesscred_launch.go). "Try again" there offers the same refusal again,
      // which is the loop the preview walked an admin into.
      setRefused(e instanceof HttpError && e.status === 409);
      setPhase("error");
      closeAuthTab();
    }
  }, [provider, startUrl, closeAuthTab]);

  // startingSentenceOf is the substrate's sentence for this read, or "". The lead-in
// below promises the reader words after it, so BOTH arms that use it check for
// "" first and fall through — to the clock, or to the run's own failure_hint —
// rather than putting a promise on screen with nothing behind it (review S2).
// run-status-detail.ts already guarantees a terminal reason gets a sentence, so
// this is defence in depth against a future daemon, not a reachable state today.
function startingSentenceOf(run: AgentRun | undefined): string {
  return statusDetailSentence(run?.status_detail, run?.status_reason);
}

// pollRun is the `starting` phase's whole machine: ask the run whether it is
  // up yet, mount the terminal when it is, and end the wait honestly when the
  // run ends instead. Nothing here attaches — AttachTerminal's own mint is
  // owner-or-admin and one failed mint is terminal in that component, so the
  // pane must not mount it until the ticket route would actually answer.
  const pollRun = React.useCallback(async () => {
    if (!runId) return;
    const run = await runsApi.getRun(runId).catch(() => undefined);
    const now = Date.now();
    if (!run) {
      // A transient read is not an outcome; the next tick asks again. What ENDS
      // the wait is the clock below, not this counter.
      pollFailuresRef.current += 1;
      failingSinceRef.current ??= now;
    } else {
      pollFailuresRef.current = 0;
      failingSinceRef.current = null;
    }
    setStartingSentence(startingSentenceOf(run));
    const verdict = startWaitVerdict({
      now,
      startedAt: startedAtRef.current,
      failingSince: failingSinceRef.current,
      failures: pollFailuresRef.current,
      // Null on a failed read by construction — the wait then grades on the
      // clock exactly as it did in 0.7.5.
      detail: run?.status_detail ?? null,
      reason: run?.status_reason ?? null,
    });
    if (verdict === "unreadable") {
      setError(LOGIN_SANDBOX_UNREADABLE);
      setPhase("error");
      closeAuthTab();
      return;
    }
    // The wait ends on a REASON rather than a clock: the substrate has given
    // its final answer, and the five minutes that would otherwise follow it
    // would be five minutes of waiting for news that had already arrived.
    if (verdict === "stuck" && startingSentenceOf(run)) {
      setStuck(true);
      setError(`${LOGIN_SANDBOX_STUCK_LEAD_IN} ${startingSentenceOf(run)}`);
      setPhase("error");
      // The placeholder tab was FOREGROUNDED by the click and reads "this page
      // changes to your provider's sign-in page by itself" — on the one start
      // that never will. Every other exit from the wait closes it; this arm
      // returned early and left it open with the error on the tab behind it.
      closeAuthTab();
      return;
    }
    if (verdict !== waitNoteRef.current) {
      waitNoteRef.current = verdict;
      setWaitNote(verdict);
    }
    if (!run) return;
    if (run.state === "RUNNING") {
      setEverAttached(true);
      setPhase("attached");
      return;
    }
    if (isTerminalRunState(run.state)) {
      // Codex #11: the run that went STARTING -> FAILED between two polls. The
      // server keeps a TERMINAL reason on a FAILED run so this branch can still
      // say what happened — failure_hint is only dispatch's wrapper around it.
      if (isTerminalStatusReason(run.status_reason) && startingSentenceOf(run)) {
        setStuck(true);
        setError(`${LOGIN_SANDBOX_STUCK_LEAD_IN} ${startingSentenceOf(run)}`);
        setPhase("error");
        closeAuthTab(); // same reason as the STARTING arm above
        return;
      }
      // The run's OWN sentence when it has one (D9's failure_hint covers the
      // pre-agent-start class this wait actually hits: an image that would not
      // pull, a ceiling that would not resolve); never a reworded guess.
      setError(run.failure_hint || LOGIN_SANDBOX_ENDED);
      setPhase("error");
      closeAuthTab();
    }
  }, [runId, closeAuthTab]);

  // usePoll drives BACKGROUND refreshes only (its own contract), so the first
  // ask is made here — otherwise every sign-in waits a full tick on a sandbox
  // that may already be up.
  React.useEffect(() => {
    if (phase === "starting") void pollRun();
  }, [phase, pollRun]);
  usePoll(pollRun, RUN_POLL_MS, phase !== "starting");

  // The version-skew fallback, armed once on the first attach: if the sandbox
  // has not said it is signing in by the time the grace window closes, this is
  // an image that predates the self-run and nothing else will type the pair.
  // Four signals all mean "it IS signing in, do not touch it": the image's own
  // banner, a device URL already on screen, and either helper marker (a capture
  // that already landed, or one already refused).
  React.useEffect(() => {
    if (phase !== "attached" || flow.capture !== "helper" || selfRunArmedRef.current) return;
    selfRunArmedRef.current = true;
    const timer = setTimeout(() => {
      const buf = outBufRef.current;
      if (openedUrlRef.current || buf.includes(SELFRUN_MARKER)) return;
      if (flow.doneMarker && buf.includes(flow.doneMarker)) return;
      if (flow.failMarker && buf.includes(flow.failMarker)) return;
      termRef.current?.sendText(flow.cmd + "\r");
    }, SELFRUN_GRACE_MS);
    return () => clearTimeout(timer);
  }, [phase, flow]);

  // completeCapture (Codex #9): the ONE, once-only-guarded place either the
  // marker below or the background watch ends the pane. Deps deliberately
  // exclude `onDone` (B1, above) — `onDoneRef` carries it.
  const completeCapture = React.useCallback(() => {
    if (completedRef.current) return;
    completedRef.current = true;
    watchAbortRef.current?.abort();
    if (runId && !killedRef.current) {
      killedRef.current = true;
      void runsApi.killRun(runId).catch(() => {});
    }
    closeAuthTab();
    setAutoCaptured(true);
    setPhase("done");
    onDoneRef.current();
  }, [runId, closeAuthTab]);

  // saveToken stores a token (explicit from auto-capture, or the pasted field).
  const saveToken = React.useCallback(
    async (explicit?: string) => {
      const t = (explicit ?? token).trim();
      if (!t || savedRef.current) return;
      savedRef.current = true;
      setPhase("saving");
      setError("");
      try {
        await harnessAuthApi.harnessCredentialPaste(provider, t);
        if (runId) await runsApi.killRun(runId).catch(() => {});
        closeAuthTab(); // Finding 7a: the tab must not outlive a completed sign-in.
        setPhase("done");
        onDone();
      } catch (e) {
        setError(e instanceof Error ? e.message : String(e));
        setPhase("attached"); // stay on the terminal so they can retry the paste
        savedRef.current = false; // allow another attempt (auto or manual)
      }
    },
    [provider, token, runId, onDone, closeAuthTab],
  );

  // confirmCapture is the PHASE half of the corroboration; the rule and the
  // round trip live in capture-confirm.ts. `savedRef` is already latched by the
  // caller so a repeat marker sighting can't re-enter this while it is in
  // flight.
  //
  // The run is killed FIRST (R-7): the credential is already stored by the
  // time the helper prints its marker, so the login sandbox has no reason to
  // outlive it by a round trip, and ssotoken.go's already_captured latch
  // covers a repeat. Then "saving", so the spinner covers the wait (R-8).
  //
  // review-1 S1 — Codex #9 design point 3 STANDS: a mismatch here does NOT
  // refuse. It stays on CAPTURE_VERIFYING (with Cancel, below) and hands off
  // to the background watch, which is ALREADY running (it started when the
  // pane attached, unconditionally — see the effect below) and keeps trying
  // up to its own bound. `verifyFailSentenceRef` remembers which sentence
  // this round trip would have shown, so the watch's eventual refusal (if it
  // gives up) says the SAME thing a person watching this screen the whole
  // time would expect — S-13's property (a forged marker never confirms) is
  // unchanged, only WHEN the refusal lands.
  const confirmCapture = React.useCallback(async () => {
    if (runId && !killedRef.current) {
      killedRef.current = true;
      void runsApi.killRun(runId).catch(() => {});
    }
    setPhase("saving");
    const { confirmed, unreachable } = await confirmCaptureWithServer(provider, runId);
    if (confirmed) {
      completeCapture();
      return;
    }
    verifyFailSentenceRef.current = unreachable ? CAPTURE_CHECK_UNREACHABLE : CAPTURE_NOT_CORROBORATED;
  }, [provider, runId, completeCapture]);

  // Watch the login terminal: open the OAuth URL in a new tab, then capture and
  // save the printed token — both automatically.
  const handleOutput = React.useCallback(
    (chunk: string) => {
      outBufRef.current = (outBufRef.current + chunk).slice(-16384);
      if (!openedUrlRef.current) {
        const url = flow.capture === "helper" ? extractDeviceVerificationUrl(outBufRef.current) : extractAuthUrl(outBufRef.current);
        if (url) {
          openedUrlRef.current = true;
          setAuthUrl(url);
          // Finding 7a: NAVIGATE the tab opened on the click (launch(), top)
          // rather than opening a fresh one from this PTY callback — a
          // callback can never satisfy the gesture requirement a fresh
          // window.open would need.
          authTabRef.current?.navigate(url);
        }
      }
      // Finding 7b: a HINT; only swaps CAPTURE_HANDOFF in (checked before the
      // marker latches so it fires even when the marker never arrives).
      // review-1 S2: also wakes the watch — the CLI's own success line is
      // exactly the moment a capture becomes plausible soon.
      if (!signedInRef.current && extractSignedIn(outBufRef.current)) {
        signedInRef.current = true;
        setSignedIn(true);
        watchWakeRef.current.dispatchEvent(new Event("tick"));
      }
      if (savedRef.current || failedRef.current) return;
      // Helper-capture providers (AWS SSO): the credential is uploaded by the
      // in-sandbox helper through the brokered endpoint — it is NEVER printed, so
      // there is nothing to scrape. Watch only for the helper's success marker.
      if (flow.capture === "helper") {
        // Checked BEFORE doneMarker: a refused capture (wrong-account pin, a
        // portal error) prints the fail marker and NEVER the done marker —
        // without this the pane just sat on "waiting" forever, the only signal
        // a terminal that had quietly stopped scrolling.
        if (flow.failMarker) {
          const sentence = extractFailSentence(outBufRef.current, flow.failMarker);
          if (sentence) {
            failedRef.current = true;
            setError(`${SANDBOX_REFUSAL_LEAD_IN} ${sentence}`);
            setPhase("error");
            if (runId) void runsApi.killRun(runId).catch(() => {});
            closeAuthTab();
            return;
          }
        }
        if (flow.doneMarker && outBufRef.current.includes(flow.doneMarker)) {
          savedRef.current = true;
          void confirmCapture();
        }
        return;
      }
      const tok = extractSetupToken(outBufRef.current);
      if (tok) {
        setToken(tok);
        setAutoCaptured(true);
        void saveToken(tok);
      }
    },
    [saveToken, confirmCapture, flow, runId, onDone, closeAuthTab],
  );

  // Bridge the pasted login code into the terminal's stdin, so the operator uses
  // a normal input field with native paste instead of the terminal's Ctrl+Shift+V.
  const sendCode = React.useCallback(() => {
    const c = code.trim();
    if (!c) return;
    termRef.current?.sendText(c + "\r");
    setCode("");
  }, [code]);

  const cancel = React.useCallback(() => {
    dismissedRef.current = true;
    if (runId) runsApi.killRun(runId).catch(() => {});
    watchAbortRef.current?.abort();
    closeAuthTab();
    onCancel();
  }, [runId, onCancel, closeAuthTab]);

  // Codex #15: the hosting dialog's Escape / overlay-close reaches `cancel` via `paneRef`.
  React.useImperativeHandle(paneRef, () => ({ cancel }), [cancel]);

  // review-1 B1: read through a ref (same reasoning as onDoneRef above) so
  // this effect's deps can drop `completeCapture` — a parent re-render must
  // never restart the watch (it would reset the 45-min deadline, the 5-min
  // post-terminal grace and the back-off to its 2s floor every time).
  const completeCaptureRef = React.useRef(completeCapture);
  React.useEffect(() => {
    completeCaptureRef.current = completeCapture;
  }, [completeCapture]);

  // The markerless path to onDone (Codex #8, #9): starts once attached, so a
  // lost marker AND a lost success line still converge. AWS/helper only. A
  // parent re-render does not restart this — only `watchEligible`/
  // flow.capture/provider/runId changing does.
  //
  // review-1 S1: eligible through BOTH "attached" and "saving" — a plain
  // boolean, not `phase` itself, so the transition INTO "saving" (confirmCapture
  // narrating the short round trip) does not toggle the effect's own identity
  // and tear the watch down mid-flight. The watch is what design point 3
  // hands the refusal to: a `false` resolution here is what actually ends
  // the wait now (`verifyFailSentenceRef` carries the short round trip's own
  // verdict, if one ran, so the sentence a person sees is the same whichever
  // path ends it — only WHEN differs, and S-13's property does not).
  const watchEligible = phase === "attached" || phase === "saving";
  React.useEffect(() => {
    if (!watchEligible || flow.capture !== "helper" || !runId) return;
    const controller = new AbortController();
    watchAbortRef.current = controller;
    const wake = watchWakeRef.current;
    void watchForCapture({ provider, runId, signal: controller.signal, wake }).then((confirmed) => {
      if (controller.signal.aborted) return;
      if (confirmed) {
        completeCaptureRef.current();
        return;
      }
      failedRef.current = true;
      setError(verifyFailSentenceRef.current || CAPTURE_NOT_CORROBORATED);
      setPhase("error");
      closeAuthTab();
    });
    // review-1 S2: return-to-visible wakes the watch too (the same idiom
    // usePoll already uses elsewhere in this console) — a tab backgrounded
    // through part of the schedule's slower tiers should not stay quiet
    // longer than a foregrounded one would have.
    const onVisible = () => {
      if (!document.hidden) wake.dispatchEvent(new Event("tick"));
    };
    document.addEventListener("visibilitychange", onVisible);
    return () => {
      document.removeEventListener("visibilitychange", onVisible);
      controller.abort();
      // Never leave a stale (aborted) controller behind for a later
      // `cancel()`/completeCapture to read as though it were still live.
      if (watchAbortRef.current === controller) watchAbortRef.current = null;
    };
  }, [watchEligible, flow.capture, provider, runId, closeAuthTab]);

  // Finding 7a's fourth exit path: the tab must not outlive the pane.
  React.useEffect(() => closeAuthTab, [closeAuthTab]);

  return (
    <div className="space-y-3 rounded-lg border border-border bg-surface-2/40 p-3" data-testid="harness-login-pane">
      <div className="flex items-center gap-2">
        <KeyRound className="size-4 shrink-0 text-primary" />
        <span className="text-sm font-medium text-foreground">{flow.title}</span>
      </div>
      {/* The blurb narrates the RUNNING flow ("Wardyn opened a sandbox…") — on
          the intro nothing has launched yet, so the expectations list speaks
          instead and the blurb would be a lie. */}
      {phase !== "intro" && (
        <p className="text-xs leading-relaxed text-muted-foreground">{flow.blurb(startURLManaged)}</p>
      )}

      {error && (
        <div
          role="alert"
          className="flex items-start gap-2 rounded-lg border border-warning/30 bg-warning-subtle px-3 py-2 text-xs text-warning"
        >
          <TriangleAlert className="mt-0.5 size-4 shrink-0" />
          <p>{error}</p>
        </div>
      )}

      {phase === "intro" && (
        <div className="space-y-3" data-testid="login-intro">
          <ExpectList items={flow.expects} />
          {/* The portal the sign-in will use is the row's, not one to type. */}
          {flow.needsStartUrl && startURLManaged && (
            <p className="text-xs leading-relaxed text-muted-foreground">{AGENTS.SSO_START_URL_MANAGED}</p>
          )}
          <div className="flex flex-wrap gap-2">
            <Button size="sm" onClick={() => void launch()}>
              <KeyRound className="size-3.5" /> Start login
            </Button>
            <Button size="sm" variant="outline" onClick={onCancel}>
              Cancel
            </Button>
          </div>
        </div>
      )}

      {phase === "prompt" && (
        <div className="space-y-2" data-testid="login-start-url-prompt">
          <ExpectList items={flow.expects} />
          <label className="block text-xs font-medium text-foreground" htmlFor="harness-login-start-url">
            Your AWS access portal URL
          </label>
          <div className="flex flex-wrap items-center gap-2">
            <Input
              id="harness-login-start-url"
              value={startUrl}
              onChange={(e) => setStartUrl(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter" && isLikelyStartUrl(startUrl)) void launch();
              }}
              placeholder="https://my-org.awsapps.com/start"
              className="h-9 min-w-[18rem] flex-1 font-mono"
              aria-label="AWS access portal start URL"
            />
            <Button size="sm" onClick={() => void launch()} disabled={!isLikelyStartUrl(startUrl)}>
              <KeyRound className="size-3.5" /> Start login
            </Button>
            <Button size="sm" variant="outline" onClick={onCancel}>
              Cancel
            </Button>
          </div>
          <p className="text-xs text-muted-foreground">
            Find it in the AWS access portal (IAM Identity Center) — it looks like{" "}
            <code className="rounded bg-background/70 px-1 py-0.5 font-mono">https://my-org.awsapps.com/start</code>.
            Wardyn does not store it; the SSO region comes from the daemon&apos;s{" "}
            <code className="rounded bg-background/70 px-1 py-0.5 font-mono">-bedrock-aws-sso-region</code> /{" "}
            <code className="rounded bg-background/70 px-1 py-0.5 font-mono">-bedrock-region</code> setting.
          </p>
        </div>
      )}

      {phase === "launching" && (
        <p className="flex items-center gap-2 text-xs text-muted-foreground">
          <Loader2 className="size-3.5 animate-spin" /> Opening the login sandbox…
        </p>
      )}

      {/* The wait, with the one control that matters during it: Cancel kills
          the run by the id the POST already handed back, so a sandbox stuck on
          a cold pull is the operator's to end rather than the idle cap's. */}
      {phase === "starting" && (
        <div className="flex flex-wrap items-center gap-2" data-testid="login-sandbox-starting">
          <p role="status" className="flex flex-1 items-center gap-2 text-xs leading-relaxed text-muted-foreground">
            <Loader2 className="size-3.5 shrink-0 animate-spin" />{" "}
            {/* A reason always beats the clock's hedged guess; with none to
                read this is 0.7.5's ladder byte for byte. */}
            {waitNote === "retrying"
              ? LOGIN_SANDBOX_READ_RETRYING
              : waitNote === "slow"
                ? startingSentence || LOGIN_SANDBOX_SLOW_START
                : LOGIN_SANDBOX_STARTING}
          </p>
          <Button size="sm" variant="outline" onClick={cancel}>
            <Square className="size-3.5" /> Cancel
          </Button>
        </div>
      )}

      {phase === "error" && (
        <div className="flex flex-wrap gap-2">
          {/* U-11: suppressed on a 409 — the refusal above already says why, and
              a retry earns the identical answer. Cancel remains the way out. */}
          {!refused && !stuck && (
            <Button size="sm" onClick={() => void launch()}>
              <KeyRound className="size-3.5" /> Try again
            </Button>
          )}
          {/* `cancel`, not a bare onCancel: an error can now arrive while the
              sandbox is still ALIVE — the wait ended because Wardyn stopped
              being able to read the run, not because the run stopped — and
              backing out of that without a kill is the orphan P5 exists to end.
              Harmless on the arms that already killed it (killRun on a dead run
              is a caught no-op) and on a launch that never got an id. */}
          <Button size="sm" variant="ghost" onClick={cancel}>
            Cancel
          </Button>
        </div>
      )}

      {/* A refused helper capture keeps the terminal mounted, read-only in
          effect: the run is already killed (the failMarker branch above), so
          the socket just closes — but the scrollback (device-code chatter,
          the portal's reply, the helper's own preceding lines) stays on
          screen beside the alert instead of vanishing with it, since the
          extracted sentence is the operator's only other artifact. The
          interactive bits below (paste boxes, the helper's own Cancel) are
          suppressed in error phase — Try again/Cancel above already cover it. */}
      {(phase === "attached" || phase === "saving" || (phase === "error" && flow.capture === "helper" && everAttached)) && runId && (
        <div className="space-y-2">
          {authUrl && phase !== "error" && (
            <a
              href={authUrl}
              target="_blank"
              rel="noopener noreferrer"
              className="inline-flex items-center gap-1.5 rounded-md border border-primary/40 bg-primary/10 px-2.5 py-1.5 text-xs font-medium text-primary hover:bg-primary/20"
              data-testid="auth-url-link"
            >
              <ExternalLink className="size-3.5" />{" "}
              {flow.capture === "helper" ? "Open the AWS verification page ↗" : "Open the Claude login page ↗"}
            </a>
          )}
          {/* Finding 7a: shown only once a link exists AND the tab was blocked. */}
          {authUrl && phase !== "error" && tabBlocked && (
            <p className="text-xs text-muted-foreground" data-testid="auth-tab-blocked-note">
              {AUTH_TAB_BLOCKED_NOTE}
            </p>
          )}
          <AttachTerminal
            ref={termRef}
            runId={runId}
            /* helper flows (aws): the sandbox runs the pair itself — see
               SELFRUN_MARKER above. Every other provider still auto-types. */
            autoRun={flow.capture === "helper" ? undefined : flow.cmd}
            onOutput={handleOutput}
            ptyCols={LOGIN_PTY_COLS}
            heightClass="h-96"
          />
          {phase === "error" ? null : phase === "saving" && !autoCaptured && flow.capture === "helper" ? (
            /* R-8: the corroboration round trip, narrated. Deliberately NOT the
               "captured" note below — the server has not agreed yet. Helper
               flows only: a scrape flow's `saving` with no autoCapture is a
               manual token paste, which keeps its own row (and its own Save
               spinner) and would be told about an AWS sign-in it never made. */
            /* A live region, like the error path's role="alert" beside
               it — a note that narrates a silent round trip is silence again
               for a screen-reader user. role="status" (polite), not "alert":
               this is one bounded fetch, not the Agents banner's poll loop. */
            <div className="flex flex-wrap items-center gap-2">
              <p
                role="status"
                className="flex flex-1 items-center gap-2 text-xs text-muted-foreground"
                data-testid="capture-verifying-note"
              >
                <Loader2 className="size-3.5 animate-spin" /> {CAPTURE_VERIFYING}
              </p>
              {/* review-1 S1 (design point 3): the SHORT round trip disagreeing
                  no longer ends the pane — the background watch keeps trying,
                  up to its own bound (terminal + 5 min grace, or 45 min
                  absolute). Without a way out here, a forged marker parked a
                  person on this note with nothing to do for however long that
                  takes. `cancel` kills the run, aborts the watch and closes
                  the tab — the same button does that everywhere else. */}
              <Button size="sm" variant="outline" onClick={cancel}>
                <Square className="size-3.5" /> Cancel
              </Button>
            </div>
          ) : autoCaptured ? (
            <p className="flex items-center gap-2 text-xs text-success" data-testid="auto-capture-note">
              {phase === "saving" ? <Loader2 className="size-3.5 animate-spin" /> : <ShieldCheck className="size-3.5" />}
              {flow.capture === "helper"
                ? "SSO session captured — connecting…"
                : "Token detected — connecting your subscription…"}
            </p>
          ) : flow.capture === "helper" ? (
            /* Device-code flow: the code is entered on the AWS verification PAGE,
               not in the terminal, and the credential is uploaded by the in-sandbox
               helper — so there is no code field and nothing to paste here.
               Finding 7b: swaps to CAPTURE_HANDOFF once `signedIn` (a hint). */
            <div className="flex flex-wrap items-center gap-2">
              <p className="flex-1 text-xs leading-relaxed text-muted-foreground" data-testid="helper-flow-note">
                {signedIn
                  ? CAPTURE_HANDOFF
                  : "In the tab that opened (or the link above), enter the user code shown in the terminal and approve. " +
                    "Wardyn captures the session automatically when the login completes."}
              </p>
              <Button size="sm" variant="outline" onClick={cancel}>
                <Square className="size-3.5" /> Cancel
              </Button>
            </div>
          ) : (
            <div className="space-y-2">
              {/* Primary interaction: paste the login-page code; we type it into
                  the terminal's stdin so the operator never needs Ctrl+Shift+V. */}
              <label className="block text-xs font-medium text-foreground" htmlFor="harness-login-code">
                Paste the code from the login page
              </label>
              <div className="flex flex-wrap items-center gap-2">
                <Input
                  id="harness-login-code"
                  value={code}
                  onChange={(e) => setCode(e.target.value)}
                  onKeyDown={(e) => {
                    if (e.key === "Enter") sendCode();
                  }}
                  placeholder="paste the code Claude gave you, then press Enter"
                  className="h-9 min-w-[18rem] flex-1 font-mono"
                  aria-label="login code"
                />
                <Button size="sm" onClick={sendCode} disabled={!code.trim()}>
                  <CornerDownLeft className="size-3.5" /> Send code
                </Button>
                <Button size="sm" variant="outline" onClick={cancel}>
                  <Square className="size-3.5" /> Cancel
                </Button>
              </div>
              {/* Fallback: paste the final token directly if auto-capture missed. */}
              <details className="text-xs text-muted-foreground">
                <summary className="cursor-pointer select-none py-1">Token didn&apos;t auto-capture?</summary>
                <div className="mt-1 flex flex-wrap items-center gap-2">
                  <Input
                    value={token}
                    onChange={(e) => setToken(e.target.value)}
                    onKeyDown={(e) => {
                      if (e.key === "Enter") void saveToken();
                    }}
                    placeholder="paste the sk-ant-oat… token"
                    className="h-9 min-w-[18rem] flex-1 font-mono"
                    aria-label="setup-token"
                    type="password"
                  />
                  <Button
                    size="sm"
                    variant="secondary"
                    onClick={() => void saveToken()}
                    disabled={phase === "saving" || !token.trim()}
                  >
                    {phase === "saving" ? (
                      <Loader2 className="size-3.5 animate-spin" />
                    ) : (
                      <ShieldCheck className="size-3.5" />
                    )}
                    Save token
                  </Button>
                </div>
              </details>
            </div>
          )}
        </div>
      )}

      {phase === "done" && (
        <p className="flex items-center gap-2 text-xs text-success">
          <ShieldCheck className="size-3.5" /> Token captured — {flow.doneLabel}.
        </p>
      )}
    </div>
  );
}
