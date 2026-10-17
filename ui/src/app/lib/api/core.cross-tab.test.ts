/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// DR-0020 / DR-0007 at the transport: what this tab tells the others, and what it does when they tell it.
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { FakeBroadcastChannel } from "../../../test/fake-broadcast-channel";
import { AUTH_CHANNEL, COALESCE_MS, resetAuthSyncForTest } from "./auth-sync";
import {
  IDENTITY_CHECK_WAIT_MS,
  identityCheckSettled,
  listenForOtherTabs,
  onAuthChange,
  onUnauthorized,
  setSignedOutHold,
  setToken,
  wfetch,
} from "./core";

const fetchMock = vi.fn<typeof fetch>();
const changed = vi.fn();
const unauthorized = vi.fn();
let stops: Array<() => void> = [];
const posted = () => FakeBroadcastChannel.posted.map((p) => (p.data as { kind: string }).kind);
const fromAnotherTab = (kind = "changed", seq = 1) =>
  FakeBroadcastChannel.deliver(AUTH_CHANNEL, { v: 1, tab: "other", seq, at: Date.now(), kind });

beforeEach(() => {
  vi.useFakeTimers();
  localStorage.clear();
  sessionStorage.clear();
  vi.stubGlobal("BroadcastChannel", FakeBroadcastChannel);
  FakeBroadcastChannel.reset();
  resetAuthSyncForTest();
  fetchMock.mockReset();
  fetchMock.mockImplementation(async () => new Response("{}", { status: 200 }));
  vi.stubGlobal("fetch", fetchMock);
  changed.mockClear();
  unauthorized.mockClear();
  onUnauthorized(unauthorized);
  stops = [onAuthChange(changed)];
});
afterEach(() => {
  for (const stop of stops) stop();
  onUnauthorized(() => {});
  setSignedOutHold(false);
  identityCheckSettled();
  resetAuthSyncForTest();
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

it("tells the other tabs of a deliberate sign-out once the server has answered it, and of a mode switch or token change as a change", async () => {
  await wfetch("/auth/logout", { method: "POST", endsSession: true });
  await wfetch("/me/view", { method: "POST" });
  setToken("first");
  setToken("first");
  setToken(null);
  expect(posted()).toEqual(["signed_out", "changed", "changed", "changed"]);
});

it("tells nobody of a logout the server never answered, though this tab still re-checks", async () => {
  fetchMock.mockRejectedValueOnce(new TypeError("offline"));
  await expect(wfetch("/auth/logout", { method: "POST", endsSession: true })).rejects.toThrow("offline");
  expect(posted()).toEqual([]);
  expect(changed).toHaveBeenCalledTimes(1);
});

it("an event from another tab invalidates this tab's session reads, once for a burst, and is not announced on", () => {
  stops.push(listenForOtherTabs());
  fromAnotherTab("changed", 1);
  fromAnotherTab("signed_out", 2);
  expect(changed).not.toHaveBeenCalled();
  vi.advanceTimersByTime(COALESCE_MS + 1);
  expect(changed).toHaveBeenCalledTimes(1);
  expect(posted()).toEqual([]);
});

it("holds a write until the identity check settles; a read is never held", async () => {
  stops.push(listenForOtherTabs());
  fromAnotherTab();
  vi.advanceTimersByTime(COALESCE_MS + 1);
  await wfetch("/runs");
  expect(fetchMock).toHaveBeenCalledTimes(1);

  const write = wfetch("/stub-save", { method: "PUT", body: "{}" });
  await vi.advanceTimersByTimeAsync(1000);
  expect(fetchMock).toHaveBeenCalledTimes(1);
  identityCheckSettled();
  await write;
  expect(fetchMock).toHaveBeenCalledTimes(2);
});

it("a write held for the check is refused, unsent, when the check ends in the signed-out hold", async () => {
  stops.push(listenForOtherTabs());
  fromAnotherTab();
  vi.advanceTimersByTime(COALESCE_MS + 1);
  const write = wfetch("/stub-save", { method: "PUT", body: "{}", save: "note" });
  const refused = expect(write).rejects.toMatchObject({ status: 401 });
  await vi.advanceTimersByTimeAsync(10);
  setSignedOutHold(true);
  identityCheckSettled();
  await refused;
  expect(fetchMock).not.toHaveBeenCalled();
  expect(unauthorized).toHaveBeenCalledExactlyOnceWith({ write: true, save: "note" });
});

it("a check that never answers refuses the write at the bound, unsent: the draft is kept and the prompt opens", async () => {
  stops.push(listenForOtherTabs());
  fromAnotherTab();
  vi.advanceTimersByTime(COALESCE_MS + 1);
  const write = wfetch("/stub-save", { method: "PUT", body: "{}", save: "note" });
  const refused = expect(write).rejects.toMatchObject({ status: 401 });
  await vi.advanceTimersByTimeAsync(IDENTITY_CHECK_WAIT_MS - 100);
  expect(unauthorized).not.toHaveBeenCalled();
  await vi.advanceTimersByTimeAsync(200);
  await refused;
  expect(fetchMock).not.toHaveBeenCalled();
  expect(unauthorized).toHaveBeenCalledExactlyOnceWith({ write: true, save: "note" });
});

it("the logout is the one write the hold does not wait for", async () => {
  stops.push(listenForOtherTabs());
  fromAnotherTab();
  vi.advanceTimersByTime(COALESCE_MS + 1);
  await wfetch("/auth/logout", { method: "POST", endsSession: true });
  expect(fetchMock).toHaveBeenCalledTimes(1);
});
