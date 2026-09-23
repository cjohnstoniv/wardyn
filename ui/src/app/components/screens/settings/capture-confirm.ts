/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The corroboration — the sign-in pane's answer to "the sandbox says it
// captured a credential; did it?"
//
// Extracted from harness-login-pane.tsx (R1-F7): the rule
// (serverConfirmsCapture), its reasoning and its sentences are that file's
// own, moved verbatim. What the pane keeps is the phase machine that calls
// this.
//
// A pure module on purpose — no React, no component state — so the forgery rule
// is exercisable as a function, which is how S-13's pins already read it.
import { audit as auditApi } from "../../../lib/api/audit";
import { runs as runsApi } from "../../../lib/api/runs";
import { setup as setupApi } from "../../../lib/api/setup";
import { isTerminalRunState, type SetupStatus } from "../../../lib/types";

// DRAFT (M2 canon pending) — S-13 (blind security review, lens-S.md): the PTY
// success marker is sandbox-forgeable by construction (a replaced/malicious
// login image can print it without ever completing a real capture), so the
// pane no longer trusts the marker alone. On doneMarker it re-fetches
// /setup/status and only claims a capture when the server independently
// agrees (serverConfirmsCapture below) — the server is the one copy of the
// truth the sandbox cannot write. This is the sentence shown when the two
// disagree.
//
// finding 7: a person whose interrupted first attempt was just retried
// successfully must not be told to do the thing they just did — but the pane
// cannot tell the two causes (an interrupted retry vs. a genuine mismatch)
// apart, so the sentence names both. Exported so ui/e2e and the tests assert
// through the constant rather than re-typing it.
//
// (W6 blind lens): who is reading it. This pane has three mounts and two of
// them are the admin's (agents-tab.tsx, connection-cards.tsx), so the copy
// must not name a specific page ("sign in again from Getting Started") or
// address a specific audience ("tell your admin") — the retry instruction
// says only to sign in again, which is true wherever this renders.
//
// Copy must not promise "starting a new sign-in closes the old one": the
// supersede it would describe (harnesscred_supersede.go) is not guaranteed
// during a rolling upgrade, when a pre-0.7.5 replica can still answer the
// same call without it. The audit trail is what a reader can actually check
// either way.
export const CAPTURE_NOT_CORROBORATED =
  "The sandbox reported a capture the server does not have. If your last attempt was interrupted, sign in again. " +
  "If it keeps happening, the sandbox's report and the server disagree — check the run's audit trail.";
// DRAFT (M2 canon pending) — R-3: getSetupStatus RESOLVES a synthetic
// `unreachable` payload for a 5xx or a dropped socket, it does not throw. An
// honest capture that DID land would then be accused of not existing. One
// retry, then this: the check failed, not the sign-in.
export const CAPTURE_CHECK_UNREACHABLE = "Wardyn couldn't reach the server to verify this sign-in — try again.";
// DRAFT (M2 canon pending) — R-8: the corroboration is a round trip the
// operator otherwise experiences as a terminal that stopped scrolling. Says
// what is happening WITHOUT claiming the capture the server has not confirmed.
export const CAPTURE_VERIFYING = "Checking with Wardyn that the session was stored…";
// One retry of an unreachable corroboration read, then give up. Not a
// propagation wait: the helper prints its marker only after the server has
// answered 204, so the store write strictly precedes the marker byte. This
// covers transport only.
const CONFIRM_RETRY_MS = 500;

// …and up to three re-reads of a status that ANSWERED but does not yet show
// this run's capture (finding 7). The marker strictly follows the 204, so the
// write precedes the read in real time — but "precedes" is not "is visible to":
// a read served by a lagging replica, or a supersede's kill landing between the
// two, produces a status that is honest and stale. Refusing on the first such
// read is what told a person whose retry WORKED to sign in again. Three reads
// over 1.5s is longer than any such gap and short enough that a genuinely forged
// marker (which never becomes true) still ends in the refusal.
const CAPTURE_CONFIRM_RETRIES = 3;

// serverConfirmsCapture corroborates a doneMarker sighting against the
// server's own /setup/status (S-13): the marker is a PTY string a forged
// sandbox binary can print unconditionally, so it is never sufficient on its
// own.
//
// It has to prove THIS sign-in captured something, not that a credential
// exists (R-1) — otherwise every re-login over an existing row, the case the
// product's own "re-run the login" fix line creates, re-admits a forged
// marker. `source_run_id` is the proof: the server stamps it from the login
// run's own token claims (ssotoken.go), nothing in the sandbox can write it,
// and an honest capture replaces the blob, so after one it always equals this
// run's id. A row carrying SOMEONE ELSE'S run id is therefore a previous
// sign-in, and refuses.
//
// The presence legs below are the FALLBACK, for a daemon old enough to omit
// source_run_id entirely. For aws that is model_access reading live/expiring
// — never the harness row, whose `captured` bit stays true for a dead
// credential (R-2). anthropic has no per-caller model_access shape here, so
// its own harness row is all it has.
//
// Only the aws flow reaches this today (R-5): it is the only flow with a
// doneMarker. anthropic ends through saveToken (capture: "scrape"). Its
// branch is kept because it is the right rule for the next helper flow.
//
// `strict` (Finding 7b, Codex #9): the background watch below is a
// FREE-RUNNING poll with no marker to anchor it, so the presence fallbacks
// below — right for the SHORT, marker-triggered round trip, where a marker
// was at least seen — would be the one dangerous line in the lane if the
// watch used them: a pre-existing credential would confirm a sign-in that
// never happened. `strict` refuses them outright; every existing caller
// (this file's own round trip, every S-13 pin) omits it and is unchanged.
// Exported for tests.
export function serverConfirmsCapture(
  status: SetupStatus,
  provider: string,
  runId?: string | null,
  opts?: { strict?: boolean },
): boolean {
  const rows = (status.harness ?? []).filter((h) => h.provider === provider);
  if (runId && rows.some((h) => h.captured && h.source_run_id === runId)) return true;
  if (opts?.strict) return false;
  if (rows.some((h) => h.source_run_id)) return false; // someone else's sign-in
  if (provider === "aws") {
    const state = status.model_access?.state;
    return state === "live" || state === "expiring";
  }
  return rows.some((h) => h.captured);
}

// confirmCaptureWithServer is the round trip itself: read /setup/status, give a
// stale-but-honest answer a moment to catch up, and report what the server
// actually agrees to.
//
// Three outcomes, not two. A DISAGREEMENT is the forgery this exists for. A
// THROW (a propagated 401) is still fail-closed — never assume the marker was
// honest because the check failed, and never retried: it is an answer, not a
// blip. An UNREACHABLE payload is neither: it is the synthetic body
// getSetupStatus resolves for a 5xx or a dropped socket, and an honest capture
// that DID land must not be accused of not existing — retry once, then say the
// check failed (R-3).
export async function confirmCaptureWithServer(
  provider: string,
  runId?: string | null,
): Promise<{ confirmed: boolean; unreachable: boolean }> {
  // A THROW stays fail-closed and is never retried: a propagated 401 is an
  // answer, not a blip, and retrying it would only delay the refusal.
  const read = async (): Promise<SetupStatus | null> => {
    try {
      return await setupApi.getSetupStatus();
    } catch {
      return null;
    }
  };
  let status = await read();
  if (status?.unreachable) {
    await new Promise((r) => setTimeout(r, CONFIRM_RETRY_MS));
    status = await read();
  }
  // The read tolerates its own write (finding 7). The read above succeeded and
  // simply shows no row for this run yet — or shows the row of a sign-in the
  // supersede is in the middle of ending. Both converge within a tick or two,
  // so re-read before accusing the sandbox. A forged marker never converges
  // and still lands on the refusal 1.5s later.
  for (let i = 0; i < CAPTURE_CONFIRM_RETRIES; i++) {
    if (!status || status.unreachable || serverConfirmsCapture(status, provider, runId)) break;
    await new Promise((r) => setTimeout(r, CONFIRM_RETRY_MS));
    status = await read();
  }
  if (status && !status.unreachable && serverConfirmsCapture(status, provider, runId)) {
    return { confirmed: true, unreachable: false };
  }
  return { confirmed: false, unreachable: !!status?.unreachable };
}

// Finding 7b: the pane sits on a sign-in that worked
//
// What was actually missing (reconciled): the corroboration above already
// shipped in 0.7.5. The gap is one step earlier — the window between the
// CLI's own success line and the helper's marker, where the pane still showed
// "open the link, enter the code, approve" — and a path to `onDone` that does
// not depend on that marker byte reaching the browser at all.

// extractSignedIn is a HINT, never a verdict — the CLI's own wording, not
// Wardyn's, matched loosely (case-insensitive substring) so a version bump
// does not silently stop it working. It authorises NOTHING: the caller may
// swap one sentence (CAPTURE_HANDOFF) and start ONE background watch: the
// watch below is what actually confirms anything.
export function extractSignedIn(buf: string): boolean {
  return /successfully logged into/i.test(buf);
}

// DRAFT (M2 canon pending) — round-2 UX S10 + nits: no "Signed in." verdict on
// forgeable evidence (the CLI's line, like the done marker, is a PTY string —
// S-13's own reasoning, applied one step earlier). chooseAccountRole prompts
// TWICE when the roster row carries no account/role pin (`wardyn: account
// [1-N]:` then `wardyn: role [1-N]:`, cmd/wardyn-aws-sso/main.go) — named here
// so a person is not left staring at a terminal that "finished".
export const CAPTURE_HANDOFF =
  "The sign-in tool reports you are signed in. Wardyn is waiting for the sandbox to hand over your session. If the terminal above lists accounts or roles, click or tab into it, type the number you want and press Enter — it may ask twice, account then role.";

// The audit action ssotoken.go emits synchronously after the store write
// (handleUploadSSOToken) — a member can read their own run's trail, and this
// is exact by construction: THIS run's capture, or nothing.
const CAPTURE_AUDIT_ACTION = "harness.credential.capture";

// Codex #9: the ceiling on the watch's OWN life, independent of the run's —
// AutoStopAfterSec is an IDLE limit (attach keepalives extend it), not a
// login lifetime, and an outage can keep the watch from ever observing a
// terminal run.
export const CAPTURE_WATCH_MAX_MS = 45 * 60_000;

// Mirrors terminalUploadGrace (internal_live_run.go) — the same window the
// server itself grants an upload after a run goes terminal.
export const CAPTURE_POST_RUN_GRACE_MS = 5 * 60_000;

// The audit hint's own back-off — fast while a capture
// is plausible soon, slower once it is not. `/setup/status` piggybacks on a
// hit immediately; otherwise it falls back to its own slow cadence — see
// CAPTURE_WATCH_STATUS_FALLBACK_MS in watchForCapture below.
const CAPTURE_WATCH_HINT_SCHEDULE: ReadonlyArray<readonly [afterMs: number, everyMs: number]> = [
  [0, 2_000],
  [30_000, 3_000],
  [2 * 60_000, 10_000],
  [10 * 60_000, 30_000],
];
const CAPTURE_WATCH_STATUS_FALLBACK_MS = 30_000;

function hintIntervalAt(elapsedMs: number): number {
  let ms = CAPTURE_WATCH_HINT_SCHEDULE[0][1];
  for (const [after, every] of CAPTURE_WATCH_HINT_SCHEDULE) {
    if (elapsedMs >= after) ms = every;
  }
  return ms;
}

// Real setTimeout, abort-cancellable — the same clock every test in this lane
// drives with vi.useFakeTimers, no injection needed. `wake` (review-1 S2) is
// an EventTarget whose "tick" event resolves the wait EARLY — the schedule's
// own interval is still the ceiling, `wake` only shortens it.
function sleep(ms: number, signal: AbortSignal, wake?: EventTarget): Promise<void> {
  return new Promise((resolve) => {
    if (signal.aborted) return resolve();
    const done = () => {
      clearTimeout(t);
      wake?.removeEventListener("tick", done);
      signal.removeEventListener("abort", done);
      resolve();
    };
    const t = setTimeout(done, ms);
    signal.addEventListener("abort", done, { once: true });
    wake?.addEventListener("tick", done, { once: true });
  });
}

// watchForCapture (Codex #8, #9) is the markerless path to `onDone`: a
// background watch that starts the moment the pane attaches, independent of
// any PTY hint, so a sign-in whose marker AND success line are both lost
// still converges instead of waiting forever. Two reads, two jobs:
//   · GET /audit?run_id=&action=harness.credential.capture — a WAKE-UP HINT
//     ONLY (Codex #8): ssotoken.go emits this audit row best-effort AFTER the
//     store write, so an audit-first watcher could miss a genuinely stored
//     capture forever if it trusted silence. A hit only makes the
//     authoritative check below run sooner.
//   · GET /setup/status, read STRICT — the only thing that can end this loop
//     successfully. Every read failure (either endpoint) is a TICK, never a
//     verdict: the loop's only two exits are a strict confirmation (true) and
//     running out of time (false).
// Bounded by the run's own life (terminal + CAPTURE_POST_RUN_GRACE_MS) OR the
// absolute CAPTURE_WATCH_MAX_MS, whichever comes first.
//
// `wake` (review-1 S2, the back-off schedule's "immediate tick" arm): an
// EventTarget the caller can dispatch a `new Event("tick")` on to cut the
// CURRENT wait short — the CLI-line hint and a return-to-visible both do
// this. The run's own state transition needs no separate wiring: the loop
// already re-reads `getRun` every tick regardless.
export async function watchForCapture({
  provider,
  runId,
  signal,
  wake,
}: {
  provider: string;
  runId: string;
  signal: AbortSignal;
  wake?: EventTarget;
}): Promise<boolean> {
  const startedAt = Date.now();
  let terminalAt: number | null = null;
  let lastStatusCheck = startedAt; // the fallback's own clock starts at watch start, not at epoch 0
  while (!signal.aborted) {
    const now = Date.now();
    if (now - startedAt >= CAPTURE_WATCH_MAX_MS) return false;
    if (terminalAt !== null && now - terminalAt >= CAPTURE_POST_RUN_GRACE_MS) return false;

    if (terminalAt === null) {
      const run = await runsApi.getRun(runId).catch(() => undefined);
      if (run && isTerminalRunState(run.state)) terminalAt = Date.now();
    }

    let hinted = false;
    try {
      hinted = (await auditApi.listAudit(runId, CAPTURE_AUDIT_ACTION)).length > 0;
    } catch {
      /* a read failure is a tick, never a verdict */
    }

    if (hinted || Date.now() - lastStatusCheck >= CAPTURE_WATCH_STATUS_FALLBACK_MS) {
      lastStatusCheck = Date.now();
      try {
        const status = await setupApi.getSetupStatus();
        if (!status.unreachable && serverConfirmsCapture(status, provider, runId, { strict: true })) return true;
      } catch {
        /* a read failure is a tick, never a verdict */
      }
    }

    if (signal.aborted) return false;
    await sleep(hintIntervalAt(Date.now() - startedAt), signal, wake);
  }
  return false;
}
