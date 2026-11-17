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

// U2-05: the refusal
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
// the first) and let the LATEST `user_code=` one win, except over a strict
// prefix of the one already kept — a genuine redraw eventually reprints the
// same tail whole, and a truncated repaint of it must not replace it. Latest,
// not longest: a buffer holding two attempts with equal-length codes must give
// the second attempt's live code, never the first's dead one (the shared cases
// in internal/api/testdata/sign_in_pane_fixtures.json, which the server's
// parser reads too). (The alternative — requiring a fixed code shape — ties
// this to AWS's current `XXXX-XXXX` format; the buffer already holds the
// evidence to pick correctly without assuming that.)
//
// Newer IAM Identity Center sends a regional sign-in page instead,
// `https://<one label>.signin.aws/platform/authorize?…`, whose link carries no
// code: the CLI prints the code on its own line after "Then enter the code:".
// That link is an attempt only once that line has printed in the code's shape
// (codeLineAfter); it is never the bare fallback. The hosts, this rule and the
// latest-wins rule must agree with the server's parseSignInPane
// (internal/api/run_sign_in.go), which the shared cases pin. The hosts match
// lower-case ASCII only (no `i` flag), as the server's do.
export function extractDeviceVerificationUrl(s: string): string | null {
  return extractDeviceSignIn(s)?.url ?? null;
}

// The regional sign-in page's link alone, and the code's shape
// (signInPlatformRe and signInCodeShape in run_sign_in.go).
const PLATFORM_LINK = /^https:\/\/[a-z0-9-]+\.signin\.aws\/platform\/authorize\?/;
const CODE_SHAPE = /^[A-Za-z0-9]{1,16}(?:-[A-Za-z0-9]{1,16}){0,3}$/;
const CODE_PROMPT = "Then enter the code:";

// codeLineAfter returns the first non-empty line after a CODE_PROMPT line in
// seg (the output after a link, up to the next link), when it has the code's
// shape and its line has ended; "" otherwise, so a code still streaming is
// never taken. A row move ends a line, a cursor-forward is a space and any
// other escape is dropped, so a tmux redraw reads as its rows.
function codeLineAfter(seg: string): string {
  const lines = seg
    .replace(/\x1b\[[0-9;]*[HfdBE]/g, "\n")
    .replace(/\x1b\[[0-9;]*C/g, " ")
    .replace(ANSI_ESCAPES, "")
    .split(/\r\n|\r|\n/);
  let asked = false;
  for (const raw of lines.slice(0, -1)) {
    const line = raw.replace(/[ \t]+/g, " ").replace(/^ | $/g, "");
    if (line === "") continue;
    if (asked) return CODE_SHAPE.test(line) ? line : "";
    if (line === CODE_PROMPT) asked = true;
  }
  return "";
}

// A tmux client wider than its window gets the window's rows one by one: each
// row cleared by a move to its last column and an erase to its start
// (ROW_CLEAR, whose column is the row width), and a link longer than a row
// split at that width by a line end or the next row's clear (ROW_BREAK).
// joinRows returns the link starting at start whose first row ends at end,
// with those rows joined back, and where its last row ends. A row is joined to
// the next while it starts a line, is all ASCII and is exactly the row width
// long, and the next row starts with URL characters. printing is true while a
// full row's line end or the next row has not arrived. Its twin is
// signInJoinRows (run_sign_in.go).
const ROW_CLEAR = /\x1b\[[0-9]+;([0-9]+)H\x1b\[1K/g;
const ROW_BREAK = /^(?:\r\n|\x1b\[[0-9]+;[0-9]+H\x1b\[1K\r)/;
const URL_BODY = new RegExp(`^${URL_BODY_CHARS}+`);
function joinRows(s: string, start: number, end: number): { link: string; last: number; printing: boolean } {
  let link = s.slice(start, end);
  let last = end;
  const clears = [...s.slice(0, start).matchAll(ROW_CLEAR)];
  if (clears.length === 0 || (start > 0 && s[start - 1] !== "\n" && s[start - 1] !== "\r")) return { link, last, printing: false };
  const width = Number(clears[clears.length - 1][1]);
  let row = link;
  while (row.length === width && /^[!-~]+$/.test(row)) {
    const rest = s.slice(last);
    const br = ROW_BREAK.exec(rest)?.[0] ?? "";
    // A line end cut off at the end of what has arrived.
    if (!br) return { link, last, printing: rest === "\r" || (rest.length < 16 && !/[\r\n]/.test(rest)) };
    row = URL_BODY.exec(rest.slice(br.length))?.[0] ?? "";
    if (br.length + row.length >= rest.length) return { link, last, printing: true };
    if (!row) break;
    link += row;
    last += br.length + row.length;
  }
  return { link, last, printing: false };
}

// extractDeviceSignIn is extractDeviceVerificationUrl's link with its code:
// user_code= from a pre-filled link, the code line for a sign-in page link,
// and "" for the bare fallback. Exported for tests.
export function extractDeviceSignIn(s: string): { url: string; code: string } | null {
  const re = new RegExp(
    `https://(?:(?:device\\.sso\\.[a-z0-9-]+\\.amazonaws\\.com|[a-z0-9-]+\\.awsapps\\.com)/|[a-z0-9-]+\\.signin\\.aws/platform/authorize\\?)${URL_BODY_CHARS}*`,
    "g",
  );
  const matches = [...s.matchAll(re)];
  let bestComplete: { url: string; code: string } | null = null; // the latest attempt
  let bestBare: string | null = null; // fallback: first complete match without one
  let joined = 0;
  matches.forEach((m, i) => {
    if (m.index < joined) return; // inside the rows of a link already joined
    const end = m.index + m[0].length;
    if (end >= s.length) return; // still streaming
    const { link, last, printing } = joinRows(s, m.index, end);
    if (printing) return;
    joined = last;
    const url = link.replace(/[.,)]+$/, "");
    let code = "";
    if (url.includes("user_code=")) {
      code = /[?&]user_code=([^&#\s]+)/.exec(url)?.[1] ?? "";
    } else if (PLATFORM_LINK.test(url)) {
      code = codeLineAfter(s.slice(last, matches.slice(i + 1).find((n) => n.index >= last)?.index ?? s.length));
      if (!code) return;
    } else {
      if (!bestBare) bestBare = url;
      return;
    }
    const truncatedRepaint = !!bestComplete && url.length < bestComplete.url.length && bestComplete.url.startsWith(url);
    if (!truncatedRepaint) bestComplete = { url, code };
  });
  return bestComplete ?? (bestBare ? { url: bestBare, code: "" } : null);
}
