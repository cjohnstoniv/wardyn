/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Pure-function tests for login-pty-extract.ts's raw-PTY scrapers — no React,
// no pane state, so they need none of harness-login-pane.test.tsx's mocking
// harness. Split out to its own file (seam split, not a size-cap allowlist
// entry — see scripts/check-file-size.sh and AGENTS.md §1) once the pane's
// own test file grew past the cap.
import { describe, it, expect } from "vitest";
import { extractSetupToken, extractAuthUrl, extractDeviceVerificationUrl, extractFailSentence } from "./login-pty-extract";

// A realistic setup-token body: sk-ant-oat<2 digits>-<long url-safe blob>.
const TOKEN = "sk-ant-oat01-" + "A".repeat(60) + "-_" + "b3".repeat(10);

describe("extractSetupToken", () => {
  it("captures a complete token followed by a newline", () => {
    expect(extractSetupToken(`Your token:\n${TOKEN}\n`)).toBe(TOKEN);
  });

  it("captures a token even when ANSI/reset codes follow it", () => {
    expect(extractSetupToken(`${TOKEN}\x1b[0m\r\n`)).toBe(TOKEN);
  });

  it("does NOT capture a token still streaming at the buffer's end", () => {
    // No trailing char yet → treat as truncated, wait for more output.
    expect(extractSetupToken(`prefix ${TOKEN}`)).toBeNull();
  });

  it("returns null when there is no token", () => {
    expect(extractSetupToken("just some\r\nterminal output\n")).toBeNull();
  });

  it("ignores a too-short lookalike (not a real token)", () => {
    expect(extractSetupToken("sk-ant-oat01-short\n")).toBeNull();
  });

  it("finds the token embedded in noisy multi-line output", () => {
    const out = `\x1b[32m✓\x1b[0m Authenticated\r\nCopy this token:\r\n  ${TOKEN}  \r\nDone.`;
    expect(extractSetupToken(out)).toBe(TOKEN);
  });
});

describe("extractAuthUrl", () => {
  it("captures a claude.ai OAuth URL followed by a newline", () => {
    const url = "https://claude.ai/oauth/authorize?code=true&client_id=abc123&scope=user";
    expect(extractAuthUrl(`Visit:\r\n${url}\r\n`)).toBe(url);
  });

  it("captures a console.anthropic.com auth URL", () => {
    const url = "https://console.anthropic.com/oauth/authorize?x=1";
    expect(extractAuthUrl(`${url}\n`)).toBe(url);
  });

  it("strips trailing punctuation", () => {
    const url = "https://claude.ai/oauth/authorize?code=true";
    expect(extractAuthUrl(`Open (${url}).\n`)).toBe(url);
  });

  it("does NOT capture a URL still streaming at the buffer's end", () => {
    expect(extractAuthUrl("go to https://claude.ai/oauth/authorize?code=tru")).toBeNull();
  });

  it("ignores the token-exchange host (api.anthropic.com) and unrelated URLs", () => {
    expect(extractAuthUrl("POST https://api.anthropic.com/v1/oauth/token \n")).toBeNull();
    expect(extractAuthUrl("see https://example.com/docs \n")).toBeNull();
  });

  it("captures a full ~200-char OAuth URL on one line (login PTY is forced wide so it never wraps)", () => {
    // The login flow forces LOGIN_PTY_COLS so claude prints this on a single line;
    // response_type=code (dropped by the old narrow-PTY wrap bug) survives intact.
    const url =
      "https://claude.ai/oauth/authorize?response_type=code&client_id=abcdef0123456789&redirect_uri=https%3A%2F%2Fconsole.anthropic.com%2Foauth%2Fcode%2Fcallback&scope=org%3Acreate_api_key+user%3Aprofile&code_challenge=Zm9vYmFyYmF6cXV4&code_challenge_method=S256&state=deadbeefcafef00d";
    expect(extractAuthUrl(`Visit:\r\n${url}\r\nPaste the code here:\r\n`)).toBe(url);
    expect(new URL(extractAuthUrl(`\r\n${url}\r\n`)!).searchParams.get("response_type")).toBe("code");
  });

  // Owner field report regression (0.7.8): a CSI written with no whitespace
  // between it and the URL — the shape a terminal colorizer actually emits —
  // must not be swallowed into the match; control bytes are not `\s`.
  it("does not swallow a CSI reset that immediately follows the URL", () => {
    const url = "https://claude.ai/oauth/authorize?code=true&client_id=abc123";
    expect(extractAuthUrl(`\x1b[36m${url}\x1b[0m\r\n`)).toBe(url);
  });
});

// Owner field report (0.7.8): "open AWS sign-in" opened a tab whose URL had
// junk appended after user_code — the sandbox's tmux session redraws the
// screen with cursor-addressed CSI, and those bytes butted right up against
// the URL with no whitespace to stop the old class. These four all yield the
// SAME clean URL; the two after them pin the hazards the fix itself creates.
describe("extractDeviceVerificationUrl", () => {
  const CLEAN = "https://d-1234567890.awsapps.com/start/#/device?user_code=ABCD-EFGH";

  it("captures a CSI-colorized URL", () => {
    expect(extractDeviceVerificationUrl(`\x1b[32m${CLEAN}\x1b[0m\r\n`)).toBe(CLEAN);
  });

  it("captures a URL wrapped in an OSC-8 terminal hyperlink", () => {
    const osc8 = `\x1b]8;;${CLEAN}\x1b\\${CLEAN}\x1b]8;;\x1b\\\r\n`;
    expect(extractDeviceVerificationUrl(osc8)).toBe(CLEAN);
  });

  it("captures a URL with a tmux redraw's cursor-position sequence butted against it", () => {
    expect(extractDeviceVerificationUrl(`${CLEAN}\x1b[24;1HThen enter the code shown above.\r\n`)).toBe(CLEAN);
  });

  it("captures the plain URL with no escapes at all", () => {
    expect(extractDeviceVerificationUrl(`${CLEAN}\r\n`)).toBe(CLEAN);
  });

  // Hazard the fix itself introduces: excluding control bytes means a CSI
  // that lands INSIDE user_code= now ends the match early — visible junk
  // traded for an invisible, silently truncated code. A later, whole redraw
  // of the same tail is what the longest-match rule prefers instead.
  it("does not return a code truncated by a CSI landing mid-user_code, when the buffer also holds the complete one", () => {
    const truncated = "https://d-1234567890.awsapps.com/start/#/device?user_code=ABCD\x1b[0mEFGH\r\n";
    const buf = `${truncated}some redraw noise\r\n${CLEAN}\r\n`;
    expect(extractDeviceVerificationUrl(buf)).toBe(CLEAN);
  });

  it("does not fuse the next redrawn row into the URL, even when that row starts with URL-safe characters", () => {
    const buf = `${CLEAN}\x1b[25;1HABCD1234 is not part of the link\r\n`;
    expect(extractDeviceVerificationUrl(buf)).toBe(CLEAN);
  });
});

// wardyn-aws-sso prints `<failMarker> <sentence>\n` on a refused capture — a
// wrong-account pin, a portal error — and never prints the doneMarker in that
// case, so without this the pane just spins on "waiting" forever (Appendix A
// finding 1's fail-fast ask, pane half).
describe("extractFailSentence", () => {
  const MARKER = "wardyn: aws sso credential rejected:";

  it("returns the sentence once the line has finished printing", () => {
    expect(extractFailSentence(`${MARKER} the pinned account is not entitled to this session.\n`, MARKER)).toBe(
      "the pinned account is not entitled to this session.",
    );
  });

  it("does NOT return a still-streaming line (no trailing newline yet)", () => {
    expect(extractFailSentence(`${MARKER} the pinned acco`, MARKER)).toBeNull();
  });

  it("returns null when the marker never printed", () => {
    expect(extractFailSentence("some other terminal output\n", MARKER)).toBeNull();
  });

  it("strips a trailing carriage return (PTY line endings)", () => {
    expect(extractFailSentence(`${MARKER} refused.\r\n`, MARKER)).toBe("refused.");
  });

  // U2-05 (blind round 2, lens-U2): the 300-rune cap, the ANSI strip and the
  // marker defang all live in cmd/wardyn-aws-sso — i.e. in the binary a forged
  // login image REPLACES, which is the threat model S-13 hardened the success
  // path against. The client keeps a bound of its own so a sandbox cannot
  // paint a screenful of its own prose into Wardyn's alert.
  it("caps the sentence at 300 characters, whatever the sandbox printed", () => {
    const long = "x".repeat(5000);
    expect(extractFailSentence(`${MARKER} ${long}\n`, MARKER)).toHaveLength(300);
  });

  it("leaves a sentence within the bound untouched", () => {
    expect(extractFailSentence(`${MARKER} refused.\n`, MARKER)).toBe("refused.");
  });

  // The ANSI strip lived only in cmd/wardyn-aws-sso,
  // beside the cap U2-05 already re-applied here — same argument, same place.
  // Colour/cursor CSI and an OSC title-set are the shapes a PTY actually emits.
  it("strips ANSI CSI and OSC sequences the sandbox printed", () => {
    expect(extractFailSentence(`${MARKER} \u001b[1;31mrefused\u001b[0m.\n`, MARKER)).toBe("refused.");
    expect(extractFailSentence(`${MARKER} \u001b]0;pwned title\u0007refused.\n`, MARKER)).toBe("refused.");
  });

  // The strip runs BEFORE the cap, so escape bytes cannot spend the 300-char
  // budget on the operator's behalf.
  it("caps on visible characters, not on escape bytes", () => {
    const noisy = "\u001b[31mx\u001b[0m".repeat(400);
    expect(extractFailSentence(`${MARKER} ${noisy}\n`, MARKER)).toBe("x".repeat(300));
  });
});
