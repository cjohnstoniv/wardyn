/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// THE CORROBORATION — the sign-in pane's answer to "the sandbox says it
// captured a credential; did it?"
//
// EXTRACTED from harness-login-pane.tsx (R1-F7), which was nine lines under the
// 1000-line cap with two more lanes still to touch it. Nothing here is new: the
// rule (serverConfirmsCapture), its reasoning and its sentences are that file's
// own, moved VERBATIM. What the pane keeps is the phase machine that calls this.
//
// A pure module on purpose — no React, no component state — so the forgery rule
// is exercisable as a function, which is how S-13's pins already read it.
import { setup as setupApi } from "../../../lib/api/setup";
import type { SetupStatus } from "../../../lib/types";

// DRAFT (M2 canon pending) — S-13 (blind security review, lens-S.md): the PTY
// success marker is sandbox-forgeable by construction (a replaced/malicious
// login image can print it without ever completing a real capture), so the
// pane no longer trusts the marker alone. On doneMarker it re-fetches
// /setup/status and only claims a capture when the server independently
// agrees (serverConfirmsCapture below) — the server is the one copy of the
// truth the sandbox cannot write. This is the sentence shown when the two
// disagree.
//
// 0.7.4 field report, finding 7: the old sentence ("— sign in again.") told a
// person whose interrupted first attempt had just been retried SUCCESSFULLY to
// do the thing they had just done. The two causes need different actions from
// the human and the pane cannot tell them apart, so the sentence names both and
// what each one costs: retry (now safe — a new sign-in closes the old one,
// harnesscred_supersede.go) or escalate. Exported so ui/e2e and the tests assert
// THROUGH the constant rather than re-typing it.
export const CAPTURE_NOT_CORROBORATED =
  "The sandbox reported a capture the server does not have. If your last attempt was interrupted, sign in again from " +
  "Getting Started — starting a new sign-in closes the old one. If it keeps happening, tell your admin: the sandbox's " +
  "report and the server disagree.";
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
// TODAY ONLY THE aws FLOW REACHES THIS (R-5): it is the only flow with a
// doneMarker. anthropic ends through saveToken (capture: "scrape"). Its
// branch is kept because it is the right rule for the next helper flow.
// Exported for tests.
export function serverConfirmsCapture(status: SetupStatus, provider: string, runId?: string | null): boolean {
  const rows = (status.harness ?? []).filter((h) => h.provider === provider);
  if (runId && rows.some((h) => h.captured && h.source_run_id === runId)) return true;
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
  // THE READ TOLERATES ITS OWN WRITE (finding 7). The read above SUCCEEDED and
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
