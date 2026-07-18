/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { extractSetupToken, extractAuthUrl } from "./harness-login-pane";

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
});
