/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Extracted from harness-login-pane.tsx: these four functions
// read a chunk of raw PTY text and pull one thing out of it — no React, no
// pane state, no side effects. Moved out (with the sandbox-refusal lead-in
// and the ANSI strip that guards it) to keep the pane under the size cap.
// Re-exported from the pane so this pane's own tests (which import
// `extractSetupToken` et al. from "./harness-login-pane") keep their path.

// DRAFT (M2 canon pending) — U2-05: the refusal
// sentence below the lead-in is the sandbox's prose, printed by
// cmd/wardyn-aws-sso — the very binary a forged login image replaces (the S-13
// threat model, applied to the success path). Rendered bare inside Wardyn's
// own warning box, it would read as Wardyn's finding. This fixed, Wardyn-authored
// lead-in names the speaker; FAIL_SENTENCE_MAX bounds what the speaker gets to
// say, client-side, rather than trusting the helper's own 300-rune cap.
export const SANDBOX_REFUSAL_LEAD_IN = "The login sandbox reported:";
// Same bound wardyn-aws-sso applies (main.go), re-applied where a replaced
// image cannot reach it.
const FAIL_SENTENCE_MAX = 300;
// The same ANSI strip, for the same reason — CSI (colour, cursor),
// OSC (title/hyperlink, terminated by BEL or ST) and the bare Fe escapes. A
// sandbox that can print its own sentence can print escape bytes around it;
// stripping them here means the 300-char budget is spent on characters the
// operator actually reads, and the alert renders text rather than control
// codes. Runs before the cap.
const ANSI_ESCAPES = /(?:\][^]*(?:|\\)?|\[[0-9;:?]*[ -/]*[@-~]|[@-Z\\-_])/g;

// With the pane's login terminal forced wide (LOGIN_PTY_COLS), `claude
// setup-token` prints the OAuth URL and the sk-ant-oat token each on a
// single line, so these two single-line extractors are correct and need no
// reassembly.

// An escape sequence written immediately after a URL — no whitespace between
// them — must not be captured as part of it: a tmux redraw's cursor-addressed
// move doesn't use \r\n between rows, so a CSI can butt right up against
// `user_code=…`. Control bytes are not `\s`, so excluding them here is what a
// delimiter can't do. One shared class so extractAuthUrl and
// extractDeviceVerificationUrl can't drift onto different exclusion sets;
// each keeps its own quantifier (+/*) below.
const URL_BODY_CHARS = "[^\\s'\"<>\\x00-\\x1f\\x7f]";

// extractSetupToken pulls a complete `claude setup-token` token out of a chunk of
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
// `<marker> <sentence>`, one line, printed on a refused capture (a
// wrong-account pin, a portal error — never on success, where doneMarker
// prints instead). Same trailing-boundary rule as the other extractors: only
// returns once the line has actually finished printing (a trailing newline),
// so a still-streaming prefix is never read as the whole refusal. U2-05: and
// it is stripped of ANSI escapes and capped here, at FAIL_SENTENCE_MAX, so
// both survive a replaced login image. Exported for tests.
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
  const re = new RegExp(
    `https://(?:claude\\.ai|claude\\.com|console\\.anthropic\\.com|platform\\.claude\\.com)/${URL_BODY_CHARS}+`,
    "gi",
  );
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
//
// Excluding control bytes from the URL class means
// a CSI/OSC written mid-URL now ends the match instead of getting swallowed
// into it — but a tmux redraw can repaint the same line more than once in the
// buffer, so a CSI landing inside `user_code=…` would end a match early and
// still pass the `user_code=` check: visible junk traded for an invisible,
// silently truncated code. Chosen fix: keep every candidate seen (not just
// the first) and prefer the longest `user_code=` one — a genuine redraw
// eventually reprints the same tail whole, and the untruncated capture is the
// longer one. (The alternative — requiring a fixed code shape — ties this to
// AWS's current `XXXX-XXXX` format; the buffer already holds the evidence to
// pick correctly without assuming that.)
export function extractDeviceVerificationUrl(s: string): string | null {
  const re = new RegExp(
    `https://(?:device\\.sso\\.[a-z0-9-]+\\.amazonaws\\.com|[a-z0-9-]+\\.awsapps\\.com)/${URL_BODY_CHARS}*`,
    "gi",
  );
  let bestComplete: string | null = null; // longest user_code= candidate seen
  let bestBare: string | null = null; // fallback: first complete match without one
  let m: RegExpExecArray | null;
  while ((m = re.exec(s)) !== null) {
    if (m.index + m[0].length >= s.length) continue; // still streaming
    const url = m[0].replace(/[.,)]+$/, "");
    if (url.includes("user_code=")) {
      if (!bestComplete || url.length > bestComplete.length) bestComplete = url;
    } else if (!bestBare) {
      bestBare = url;
    }
  }
  return bestComplete ?? bestBare;
}
