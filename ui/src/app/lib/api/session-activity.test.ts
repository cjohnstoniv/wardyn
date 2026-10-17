/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterAll, beforeAll, describe, expect, it, vi } from "vitest";
import { existsSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { wfetch } from "./core";
import {
  ACTIVITY_HEADER,
  ACTIVITY_WINDOW_MS,
  listenForInput,
  onSessionExpiry,
  SESSION_EXPIRES_HEADER,
  sessionExpiry,
} from "./session-activity";

function repoRoot(): string {
  let dir = resolve(process.cwd());
  for (let i = 0; i < 8; i++) {
    if (existsSync(join(dir, "go.mod"))) return dir;
    dir = dirname(dir);
  }
  throw new Error("go.mod not found walking up from " + process.cwd());
}

// The server reads and writes these names (internal/auth/oidc/session_life.go); a rename on either
// side alone would silently turn renewal off for the console.
it("names the same headers as the server", () => {
  const go = readFileSync(join(repoRoot(), "internal/auth/oidc/session_life.go"), "utf8");
  expect(go).toContain(`activityHeader = "${ACTIVITY_HEADER}"`);
  expect(go).toContain(`sessionExpiresHeader = "${SESSION_EXPIRES_HEADER}"`);
});

describe("wfetch and the person's activity", () => {
  let stop: () => void;
  const sent = vi.fn<(init: RequestInit) => void>();
  beforeAll(() => {
    vi.useFakeTimers({ toFake: ["Date"] });
    stop = listenForInput();
    vi.stubGlobal("fetch", async (_url: string, init: RequestInit) => {
      sent(init);
      return new Response("{}", { status: 200 });
    });
  });
  afterAll(() => {
    stop();
    vi.unstubAllGlobals();
    vi.useRealTimers();
  });
  const lastActivityHeader = () => new Headers(sent.mock.lastCall?.[0].headers).get(ACTIVITY_HEADER);

  it("marks only a request sent within the window after the person's own input", async () => {
    await wfetch("/runs");
    expect(lastActivityHeader()).toBeNull();

    window.dispatchEvent(new KeyboardEvent("keydown", { key: "a" }));
    await wfetch("/runs");
    expect(lastActivityHeader()).toBe("1");

    vi.setSystemTime(Date.now() + ACTIVITY_WINDOW_MS);
    await wfetch("/runs");
    expect(lastActivityHeader()).toBeNull();

    window.dispatchEvent(new WheelEvent("wheel"));
    await wfetch("/runs");
    expect(lastActivityHeader()).toBe("1");
  });
});

describe("wfetch and the session expiry the server names", () => {
  let expires: string | null = null;
  beforeAll(() => {
    vi.stubGlobal("fetch", async () => new Response("{}", { status: 200, headers: expires ? { [SESSION_EXPIRES_HEADER]: expires } : {} }));
  });
  afterAll(() => vi.unstubAllGlobals());

  it("records it and tells listeners, and an answer naming none changes nothing", async () => {
    const heard = vi.fn();
    const off = onSessionExpiry(heard);
    const renewed = new Date(Date.now() + 3_600_000);
    expires = renewed.toISOString();
    await wfetch("/runs");
    expect(sessionExpiry()).toBe(renewed.getTime());
    expect(heard).toHaveBeenCalledTimes(1);

    expires = null;
    await wfetch("/runs");
    expect(sessionExpiry()).toBe(renewed.getTime());
    expect(heard).toHaveBeenCalledTimes(1);
    off();
  });
});
