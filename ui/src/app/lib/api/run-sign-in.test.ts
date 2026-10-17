/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { runSignIn } from "./run-sign-in";

describe("runSignIn.get", () => {
  let fetchMock: ReturnType<typeof vi.fn>;
  beforeEach(() => {
    fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
  });
  afterEach(() => vi.unstubAllGlobals());

  const answer = (body: unknown) =>
    new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });

  it("returns a waiting answer with an https link", async () => {
    const body = { state: "waiting", verification_url: "https://device.sso.us-east-1.amazonaws.com/?user_code=A-B", user_code: "A-B" };
    fetchMock.mockResolvedValueOnce(answer(body));
    await expect(runSignIn.get("run-1")).resolves.toEqual(body);
    expect(String(fetchMock.mock.calls[0][0])).toContain("/runs/run-1/sign-in");
  });

  it("returns not_waiting as is", async () => {
    fetchMock.mockResolvedValueOnce(answer({ state: "not_waiting" }));
    await expect(runSignIn.get("run-1")).resolves.toEqual({ state: "not_waiting" });
  });

  // The link comes from a sandbox's terminal: only https is ever drawn.
  it.each([
    ["http", "http://device.sso.us-east-1.amazonaws.com/?user_code=A-B"],
    ["javascript", "javascript:alert(1)"],
    ["empty", ""],
  ])("refuses a waiting answer whose link is %s", async (_n, url) => {
    fetchMock.mockResolvedValueOnce(answer({ state: "waiting", verification_url: url, user_code: "A-B" }));
    await expect(runSignIn.get("run-1")).rejects.toThrow();
  });

  it("refuses a waiting answer with no code", async () => {
    fetchMock.mockResolvedValueOnce(answer({ state: "waiting", verification_url: "https://device.sso.us-east-1.amazonaws.com/" }));
    await expect(runSignIn.get("run-1")).rejects.toThrow();
  });
});
