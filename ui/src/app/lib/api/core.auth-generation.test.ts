/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { getToken, onAuthChange, onUnauthorized, setSignedOutHold, setToken, wfetch } from "./core";

const changed = vi.fn();
const unauthorized = vi.fn();
const fetchMock = vi.fn<typeof fetch>();
beforeEach(() => {
  localStorage.clear();
  sessionStorage.clear();
  changed.mockClear();
  unauthorized.mockClear();
  fetchMock.mockReset();
  onAuthChange(changed);
  onUnauthorized(unauthorized);
  vi.stubGlobal("fetch", fetchMock);
});
afterEach(() => {
  onAuthChange(null);
  onUnauthorized(() => {});
  setSignedOutHold(false);
  vi.unstubAllGlobals();
});

it("publishes token changes after storage settles, including clearing", () => {
  const observed: (string | null)[] = [];
  onAuthChange(() => observed.push(getToken()));
  setToken("first");
  setToken("first", true);
  setToken("second");
  setToken(null);
  expect(observed).toEqual(["first", "second", null]);
});

it.each(["/auth/logout", "/me/view"])("publishes %s settlement for success, failure and an uncertain network outcome", async (path) => {
  // The logout is the one write that carries endsSession (health.ts), which is what core.ts keys on.
  const init = path === "/auth/logout" ? { method: "POST", endsSession: true as const } : { method: "POST" };
  for (const status of [200, 500]) {
    fetchMock.mockResolvedValueOnce(new Response("", { status }));
    await wfetch(path, init);
  }
  fetchMock.mockRejectedValueOnce(new TypeError("offline"));
  await expect(wfetch(path, init)).rejects.toThrow("offline");
  expect(changed).toHaveBeenCalledTimes(3);
  expect(fetchMock).toHaveBeenCalledTimes(3);
});

it.each(["/me", "/policies"])("a delayed 401 from %s preserves a new token and reports only refused writes", async (path) => {
  let resolve!: (response: Response) => void;
  fetchMock.mockImplementationOnce(() => new Promise<Response>((done) => { resolve = done; }));
  setToken("old");
  const write = path !== "/me";
  const request = wfetch(path, { method: write ? "POST" : "GET", save: write ? "policy" : undefined });
  setToken("new");
  resolve(new Response("", { status: 401 }));
  await expect(request).rejects.toMatchObject({ status: 401 });
  expect(getToken()).toBe("new");
  if (write) expect(unauthorized).toHaveBeenCalledExactlyOnceWith({ write: true, save: "policy" });
  else expect(unauthorized).not.toHaveBeenCalled();
  expect(fetchMock).toHaveBeenCalledTimes(1);
});

it("auth changes while the old 401 body drains also precede the unauthorized callback", async () => {
  let close!: () => void;
  const body = new ReadableStream({ start(controller) { close = () => controller.close(); } });
  fetchMock.mockResolvedValueOnce(new Response(body, { status: 401 }));
  const request = wfetch("/me");
  await Promise.resolve();
  setToken("new");
  close();
  await expect(request).rejects.toMatchObject({ status: 401 });
  expect(unauthorized).not.toHaveBeenCalled();
  expect(getToken()).toBe("new");
});

it("a held cookie mutation is refused unsent without a settlement notification", async () => {
  setSignedOutHold(true);
  await expect(wfetch("/me/view", { method: "POST" })).rejects.toMatchObject({ status: 401 });
  expect(changed).not.toHaveBeenCalled();
  expect(fetchMock).not.toHaveBeenCalled();
});
