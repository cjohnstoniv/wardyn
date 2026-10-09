/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { act, cleanup, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { onUnauthorized, setSignedOutHold, setToken, wfetch } from "../../lib/api/core";
import type { Me } from "../../lib/api/health";
import { useSessionPoll } from "./use-session-poll";

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => { resolve = done; });
  return { promise, resolve };
}
const alice: Me = { principal: "alice", method: "sso", operator: true, security_operator: true, role: "admin", email: "" };
const response = (body: Me = alice) => new Response(JSON.stringify(body));
const advance = (ms = 0) => act(() => vi.advanceTimersByTimeAsync(ms));
const fetchMock = vi.fn<typeof fetch>();
const unauthorized = vi.fn();

beforeEach(() => {
  vi.useFakeTimers();
  sessionStorage.clear();
  localStorage.clear();
  setSignedOutHold(false);
  onUnauthorized(unauthorized);
  unauthorized.mockClear();
  fetchMock.mockReset().mockImplementation(() => Promise.resolve(response()));
  vi.stubGlobal("fetch", fetchMock);
});
afterEach(() => {
  cleanup();
  onUnauthorized(() => {});
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

function polling(accept = vi.fn(() => true)) {
  const hook = renderHook(() => useSessionPoll(accept));
  act(() => hook.result.current.startPoll({ over: () => false, onOver: vi.fn(), keepPolling: true }));
  return { ...hook, accept };
}

describe("session reconciliation through the real transport", () => {
  it.each([200, 401])("a %i from before a cookie mutation settles is inert, with one fresh read", async (status) => {
    const old = deferred<Response>();
    fetchMock.mockImplementationOnce(() => old.promise);
    const { accept } = polling();
    await advance(1500);
    expect(fetchMock).toHaveBeenCalledTimes(1);

    await act(async () => {
      await wfetch("/me/view", { method: "POST", body: "{}" });
      window.dispatchEvent(new Event("focus"));
      document.dispatchEvent(new Event("visibilitychange"));
      setToken("replacement");
    });
    expect(fetchMock).toHaveBeenCalledTimes(2);
    const signal = fetchMock.mock.calls[0][1]?.signal;
    expect(signal?.aborted).toBe(true);
    old.resolve(status === 401 ? new Response("", { status }) : response({ ...alice, principal: "bob" }));
    await advance();
    expect(unauthorized).not.toHaveBeenCalled();
    expect(accept).toHaveBeenCalledExactlyOnceWith(alice);
    expect(fetchMock).toHaveBeenCalledTimes(3);
    await advance(10_000);
    expect(fetchMock).toHaveBeenCalledTimes(3);
  });

  it("a current 401 still holds the page, and a rejected token gets one cookie-only check", async () => {
    setToken("rejected-token");
    fetchMock.mockImplementation(() => Promise.resolve(new Response("", { status: 401 })));
    const { accept } = polling();
    await advance(1500);
    expect(unauthorized).toHaveBeenCalledTimes(2);
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(accept).not.toHaveBeenCalled();
  });

  it("focus and visible return discard an in-flight answer and coalesce a single fresh read", async () => {
    const old = deferred<Response>();
    fetchMock.mockImplementationOnce(() => old.promise);
    const { accept } = polling();
    await advance(1500);
    act(() => {
      window.dispatchEvent(new Event("focus"));
      window.dispatchEvent(new Event("focus"));
      document.dispatchEvent(new Event("visibilitychange"));
    });
    await advance(4500);
    expect(fetchMock).toHaveBeenCalledTimes(1);
    old.resolve(response({ ...alice, principal: "bob" }));
    await advance();
    expect(accept).toHaveBeenCalledExactlyOnceWith(alice);
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it("a popup closed while /me was pending invalidates that read before applying it", async () => {
    const old = deferred<Response>();
    fetchMock.mockImplementationOnce(() => old.promise);
    const accept = vi.fn(() => true);
    const { result } = renderHook(() => useSessionPoll(accept));
    let closed = false;
    act(() => result.current.startPoll({ over: () => closed, onOver: vi.fn(), keepPolling: true }));
    await advance(1500);
    closed = true;
    old.resolve(response({ ...alice, principal: "bob" }));
    await advance();
    expect(accept).toHaveBeenCalledExactlyOnceWith(alice);
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it("Retry replaces the generation while keeping a single pending read", async () => {
    const old = deferred<Response>();
    fetchMock.mockImplementationOnce(() => old.promise);
    const { result, accept } = polling();
    await advance(1500);
    act(() => result.current.startPoll({ over: () => false, onOver: vi.fn(), keepPolling: true }));
    expect(fetchMock).toHaveBeenCalledTimes(1);
    old.resolve(response({ ...alice, principal: "bob" }));
    await advance();
    expect(accept).toHaveBeenCalledExactlyOnceWith(alice);
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it("switching to a token check waits for the previous read to settle", async () => {
    const old = deferred<Response>();
    fetchMock.mockImplementationOnce(() => old.promise);
    const { result, accept } = polling();
    await advance(1500);
    act(() => result.current.submitToken("new-token"));
    expect(result.current.busy).toBe(true);
    expect(fetchMock).toHaveBeenCalledTimes(1);
    old.resolve(response({ ...alice, principal: "bob" }));
    await advance();
    expect(accept).toHaveBeenCalledExactlyOnceWith(alice);
    expect(result.current.busy).toBe(false);
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it("a cancelled watch expires even with a read pending, and late success stays inert", async () => {
    const old = deferred<Response>();
    fetchMock.mockImplementationOnce(() => old.promise);
    const accept = vi.fn(() => true);
    const onEnd = vi.fn();
    const { result } = renderHook(() => useSessionPoll(accept));
    act(() => result.current.startPoll(null, Date.now() + 3000, onEnd));
    await advance(4500);
    expect(onEnd).toHaveBeenCalledTimes(1);
    old.resolve(response());
    await advance();
    act(() => { window.dispatchEvent(new Event("focus")); });
    await advance(10_000);
    expect(accept).not.toHaveBeenCalled();
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it("unmount removes timers and refresh listeners and discards the last response", async () => {
    const old = deferred<Response>();
    fetchMock.mockImplementationOnce(() => old.promise);
    const { unmount, accept } = polling();
    await advance(1500);
    unmount();
    old.resolve(response());
    window.dispatchEvent(new Event("focus"));
    document.dispatchEvent(new Event("visibilitychange"));
    setToken("after-unmount");
    await advance(30_000);
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(accept).not.toHaveBeenCalled();
    expect(vi.getTimerCount()).toBe(0);
  });
});
