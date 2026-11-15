/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, beforeEach, expect, it, vi } from "vitest";
import {
  getToken,
  holdWritesForIdentityCheck,
  IDENTITY_CHECK_WAIT_MS,
  identityCheckSettled,
  onAuthChange,
  onUnauthorized,
  setExpectedPrincipal,
  setSignedOutHold,
  setToken,
  wfetch,
} from "./core";
import { secrets } from "./secrets";

const changed = vi.fn();
let unsubscribe: () => void;
let unsubscribeObserver: (() => void) | undefined;
const unauthorized = vi.fn();
const fetchMock = vi.fn<typeof fetch>();
beforeEach(() => {
  localStorage.clear();
  sessionStorage.clear();
  changed.mockClear();
  unauthorized.mockClear();
  fetchMock.mockReset();
  unsubscribe = onAuthChange(changed);
  onUnauthorized(unauthorized);
  vi.stubGlobal("fetch", fetchMock);
});
afterEach(() => {
  unsubscribe();
  unsubscribeObserver?.();
  onUnauthorized(() => {});
  setSignedOutHold(false);
  vi.unstubAllGlobals();
});

it("publishes token changes after storage settles, including clearing", () => {
  const observed: (string | null)[] = [];
  unsubscribeObserver = onAuthChange(() => observed.push(getToken()));
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
  await expect(wfetch(path, init)).rejects.toMatchObject({ status: 0 });
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

it("independent listeners dispose without disabling a replacement", () => {
  const second = vi.fn();
  const off = onAuthChange(second);
  setToken("first");
  unsubscribe();
  setToken("second");
  off();
  expect(changed).toHaveBeenCalledTimes(1);
  expect(second).toHaveBeenCalledTimes(2);
});

it("names the confirmed person on an asPerson draft write only (DR-0034)", async () => {
  const sent = () => (fetchMock.mock.calls.at(-1)![1]!.headers as Headers).get("X-Wardyn-Expected-Principal");
  fetchMock.mockImplementation(async () => new Response("{}", { status: 200 }));
  setExpectedPrincipal("a@example.com");
  await wfetch("/agent-providers", { method: "PUT", asPerson: true });
  expect(sent()).toBe("a%40example.com");
  await wfetch("/agent-providers", { method: "PUT" });
  expect(sent()).toBeNull();
  setExpectedPrincipal("");
  await wfetch("/agent-providers", { method: "PUT", asPerson: true });
  expect(sent()).toBeNull();
});

it("a later step of a save names the person it began under, not the one this tab has confirmed since", async () => {
  const sent = () => (fetchMock.mock.calls.at(-1)![1]!.headers as Headers).get("X-Wardyn-Expected-Principal");
  fetchMock.mockImplementation(async () => new Response("{}", { status: 200 }));
  setExpectedPrincipal("b@example.com");
  await wfetch("/permissions/grants", { method: "POST", asPerson: "a@example.com" });
  expect(sent()).toBe("a%40example.com");
  // A save begun before /me had answered knows no person of its own: its later steps name whoever is confirmed
  // when each is made, as a plain asPerson write does, and nobody only while nobody is confirmed.
  await wfetch("/permissions/grants", { method: "POST", asPerson: "" });
  expect(sent()).toBe("b%40example.com");
  setExpectedPrincipal("");
  await wfetch("/permissions/grants", { method: "POST", asPerson: "" });
  expect(sent()).toBeNull();
});

it("a write held for an identity check names the person confirmed when it was made, not the one confirmed by the time it leaves", async () => {
  // The server then refuses it rather than make it as whoever signed in while it waited.
  const sent = () => (fetchMock.mock.calls.at(-1)![1]!.headers as Headers).get("X-Wardyn-Expected-Principal");
  fetchMock.mockImplementation(async () => new Response("{}", { status: 200 }));
  setExpectedPrincipal("a@example.com");
  holdWritesForIdentityCheck();
  const held = wfetch("/secrets/npm-token", { method: "PUT", asPerson: true });
  setExpectedPrincipal("b@example.com");
  identityCheckSettled();
  await held;
  expect(sent()).toBe("a%40example.com");
  // Made before /me had answered, it names nobody then, so the person confirmed by the time it leaves is named.
  setExpectedPrincipal("");
  holdWritesForIdentityCheck();
  const early = wfetch("/secrets/npm-token", { method: "PUT", asPerson: true });
  setExpectedPrincipal("b@example.com");
  identityCheckSettled();
  await early;
  expect(sent()).toBe("b%40example.com");
  setExpectedPrincipal("");
});

it("a secret saved while writes wait for an identity check names the person who confirmed it, or is refused unsent", async () => {
  // Saved, then someone else is confirmed, then the check answers: the PUT names the first person, so the server
  // refuses it rather than store the value in the second person's secrets. A check that never answers refuses it.
  const sent = () => (fetchMock.mock.calls.at(-1)![1]!.headers as Headers).get("X-Wardyn-Expected-Principal");
  fetchMock.mockImplementation(async () => new Response(null, { status: 204 }));
  setExpectedPrincipal("a@example.com");
  holdWritesForIdentityCheck();
  const saved = secrets.setSecret("npm-token", "s3cret");
  setExpectedPrincipal("b@example.com");
  identityCheckSettled();
  await saved;
  expect(fetchMock).toHaveBeenCalledTimes(1);
  expect(sent()).toBe("a%40example.com");

  fetchMock.mockClear();
  vi.useFakeTimers();
  try {
    setExpectedPrincipal("a@example.com");
    holdWritesForIdentityCheck();
    const unanswered = secrets.setSecret("npm-token", "s3cret");
    const refusal = expect(unanswered).rejects.toMatchObject({ status: 401 });
    setExpectedPrincipal("b@example.com");
    await vi.advanceTimersByTimeAsync(IDENTITY_CHECK_WAIT_MS);
    await refusal;
  } finally {
    vi.useRealTimers();
    identityCheckSettled();
    setExpectedPrincipal("");
  }
  expect(fetchMock).not.toHaveBeenCalled();
  expect(unauthorized).toHaveBeenCalledTimes(1);
});

it("a delayed 401 from a read sent before the next sign-in signs nobody out and keeps the new token", async () => {
  let resolve!: (response: Response) => void;
  fetchMock.mockImplementationOnce(() => new Promise<Response>((done) => { resolve = done; }));
  setToken("old");
  const request = wfetch("/setup/status");
  setToken("new");
  resolve(new Response("", { status: 401 }));
  await expect(request).rejects.toMatchObject({ status: 401 });
  expect(unauthorized).not.toHaveBeenCalled();
  expect(getToken()).toBe("new");
});
