/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// EXTRACTED from harness-login-pane.tsx (review-1 S4): these four functions
// read a chunk of raw PTY text and pull one thing out of it — no React, no
// pane state, no side effects. Moved out (with the sandbox-refusal lead-in
// and the ANSI strip that guards it) to keep the pane under the size cap.
// Re-exported from the pane so this pane's own tests (which import
// `extractSetupToken` et al. from "./harness-login-pane") keep their path.

// DRAFT (M2 canon pending) — U2-05 (blind round 2, lens-U2): the refusal
// sentence below the lead-in is the SANDBOX's prose, printed by
// cmd/wardyn-aws-sso — the very binary a forged login image replaces (the S-13
// threat model, applied to the success path). Rendered bare inside Wardyn's
// own warning box it read as Wardyn's finding. This fixed, Wardyn-authored
// lead-in names the speaker; FAIL_SENTENCE_MAX bounds what the speaker gets to
// say, client-side, rather than trusting the helper's own 300-rune cap.
export const SANDBOX_REFUSAL_LEAD_IN = "The login sandbox reported:";
// Same bound wardyn-aws-sso applies (main.go), re-applied where a replaced
// image cannot reach it.
const FAIL_SENTENCE_MAX = 300;
// RV-03: and the same ANSI strip, for the same reason — CSI (colour, cursor),
// OSC (title/hyperlink, terminated by BEL or ST) and the bare Fe escapes. A
// sandbox that can print its own sentence can print escape bytes around it;
// stripping them here means the 300-char budget is spent on characters the
// operator actually reads, and the alert renders text rather than control
// codes. Runs BEFORE the cap.
// eslint-disable-next-line no-control-regex
const ANSI_ESCAPES = /(?:\][^]*(?:|\\)?|\[[0-9;:?]*[ -/]*[@-~]|[@-Z\\-_])/g;

// With the pane's login terminal forced wide (LOGIN_PTY_COLS), `claude
// setup-token` prints the OAuth URL and the sk-ant-oat token each on a
// SINGLE line, so these two single-line extractors are correct and need no
// reassembly.

// extractSetupToken pulls a COMPLETE `claude setup-token` token out of a chunk of
// terminal output. Shape: `sk-ant-oat<2 digits>-<long url-safe body>`. We only
// return a match followed by another character (newline, ANSI reset, …) — proof
// the token finished printing — so a token still streaming in (truncated at the
// buffer's end) is not captured early. Exported for tests.
export function extractSetupToken(s: string): string | null {
  const re = /sk-ant-oat\d{2}-[A-Za-z0-9_-]{40,}/g;
  let m: RegExpExecArray | null;
  while ((m = re.exec(s)) !== null) {
    if (m.index + m[0].length < s.length) return m[0];
  }
  return null;
}

// extractFailSentence pulls the sentence off wardyn-aws-sso's refusal line:
// `<marker> <sentence>`, one line, printed on a REFUSED capture (a
// wrong-account pin, a portal error — never on success, where doneMarker
// prints instead). Same trailing-boundary rule as the other extractors: only
// returns once the line has actually finished printing (a trailing newline),
// so a still-streaming prefix is never read as the whole refusal. U2-05: and
// it is stripped of ANSI escapes and capped HERE, at FAIL_SENTENCE_MAX, so
// both survive a replaced login image (RV-03). Exported for tests.
export function extractFailSentence(s: string, marker: string): string | null {
  const idx = s.indexOf(marker);
  if (idx === -1) return null;
  const rest = s.slice(idx + marker.length);
  const nl = rest.indexOf("\n");
  if (nl === -1) return null;
  return rest.slice(0, nl).replace(/\r$/, "").replace(ANSI_ESCAPES, "").trim().slice(0, FAIL_SENTENCE_MAX);
}

// extractAuthUrl pulls the `claude setup-token` OAuth authorization URL out of a
// chunk of terminal output so we can open it in a new tab. Restricted to the known
// Claude/Anthropic auth hosts (never api.anthropic.com — that's the token exchange,
// not a user-facing page). Same trailing-boundary rule as the token so a
// still-streaming URL isn't opened truncated. Exported for tests.
export function extractAuthUrl(s: string): string | null {
  const re = /https:\/\/(?:claude\.ai|claude\.com|console\.anthropic\.com|platform\.claude\.com)\/[^\s'"<>]+/gi;
  let m: RegExpExecArray | null;
  while ((m = re.exec(s)) !== null) {
    if (m.index + m[0].length < s.length) return m[0].replace(/[.,)]+$/, "");
  }
  return null;
}

// extractDeviceVerificationUrl pulls the AWS SSO device-authorization verification
// URL out of `aws sso login --no-browser --use-device-code` output. Restricted to
// the IAM Identity Center device endpoint + the org access portal; the CLI prints
// both a bare URL and (usually) a `verificationUriComplete` with ?user_code=…,
// and we prefer the complete one since it pre-fills the code. Same
// trailing-boundary rule as the others so a still-streaming URL isn't opened
// truncated. Exported for tests.
export function extractDeviceVerificationUrl(s: string): string | null {
  const re = /https:\/\/(?:device\.sso\.[a-z0-9-]+\.amazonaws\.com|[a-z0-9-]+\.awsapps\.com)\/[^\s'"<>]*/gi;
  let best: string | null = null;
  let m: RegExpExecArray | null;
  while ((m = re.exec(s)) !== null) {
    if (m.index + m[0].length >= s.length) continue; // still streaming
    const url = m[0].replace(/[.,)]+$/, "");
    // Prefer the pre-filled variant so the operator doesn't retype the code.
    if (url.includes("user_code=")) return url;
    best = url;
  }
  return best;
}
