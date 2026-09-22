/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Review finding F9's own mechanics: the about:blank-then-navigate popup
// dance, the opener severed while still same-origin, the blocked-popup
// fallback, and stopping the poll on unmount.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { renderHook, act } from "@testing-library/react";

const getMineMock = vi.fn();
vi.mock("../api/scm-access", () => ({
  scmAccess: { getMine: (...a: unknown[]) => getMineMock(...a) },
}));

import { useAdoConnect } from "./use-ado-connect";

function fakePopup() {
  return {
    closed: false,
    close: vi.fn(function (this: { closed: boolean }) {
      this.closed = true;
    }),
    opener: {} as unknown,
    location: { href: "" },
  };
}

describe("useAdoConnect", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    getMineMock.mockReset();
  });
  afterEach(() => {
    vi.useRealTimers();
    // vitest 4 reuses an existing spy on a re-spied method, so window.open's
    // calls would otherwise accumulate across tests.
    vi.restoreAllMocks();
  });

  it("opens about:blank, severs the opener, then navigates the popup to the sign-in URL", async () => {
    const popup = fakePopup();
    const openSpy = vi.spyOn(window, "open").mockReturnValue(popup as unknown as Window);
    getMineMock.mockResolvedValue([]);

    const { result } = renderHook(() => useAdoConnect());
    act(() => {
      void result.current.connect();
    });

    expect(openSpy).toHaveBeenCalledWith("about:blank", "wardyn-ado-connect", expect.stringContaining("width="));
    expect(popup.opener).toBeNull();
    expect(popup.location.href).toBe("/api/v1/scm/azure-devops/signin");
  });

  it("resolves true and closes the popup once a row reads live", async () => {
    const popup = fakePopup();
    vi.spyOn(window, "open").mockReturnValue(popup as unknown as Window);
    getMineMock.mockResolvedValue([{ state: "live" }]);

    const { result } = renderHook(() => useAdoConnect());
    let resolved: boolean | null | undefined;
    act(() => {
      void result.current.connect().then((ok) => {
        resolved = ok;
      });
    });
    expect(result.current.connecting).toBe(true);

    await act(async () => {
      await vi.advanceTimersByTimeAsync(1500);
    });

    expect(resolved).toBe(true);
    expect(popup.close).toHaveBeenCalled();
    expect(result.current.connecting).toBe(false);
  });

  it("resolves false when the popup closes before connecting", async () => {
    const popup = fakePopup();
    vi.spyOn(window, "open").mockReturnValue(popup as unknown as Window);
    getMineMock.mockResolvedValue([]);

    const { result } = renderHook(() => useAdoConnect());
    let resolved: boolean | null | undefined;
    act(() => {
      void result.current.connect().then((ok) => {
        resolved = ok;
      });
    });

    popup.closed = true;
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1500);
    });

    expect(resolved).toBe(false);
  });

  // review finding F1: null, not false — a blocked popup is neither a
  // connection nor a decline, so the launch door must keep its dialog open
  // (use-ado-launch-door.test.tsx exercises that through the real door).
  it("a blocked popup (window.open returns null) sets blockedUrl and resolves null, with no polling", async () => {
    vi.spyOn(window, "open").mockReturnValue(null);

    const { result } = renderHook(() => useAdoConnect());
    let resolved: boolean | null | undefined;
    await act(async () => {
      resolved = await result.current.connect();
    });

    expect(resolved).toBe(null);
    expect(result.current.blockedUrl).toBe("/api/v1/scm/azure-devops/signin");
    expect(result.current.connecting).toBe(false);
    expect(getMineMock).not.toHaveBeenCalled();
  });

  it("stops polling on unmount — no further scmAccess.getMine() calls", async () => {
    const popup = fakePopup();
    vi.spyOn(window, "open").mockReturnValue(popup as unknown as Window);
    getMineMock.mockResolvedValue([]);

    const { result, unmount } = renderHook(() => useAdoConnect());
    act(() => {
      void result.current.connect();
    });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1500);
    });
    const callsBeforeUnmount = getMineMock.mock.calls.length;
    expect(callsBeforeUnmount).toBeGreaterThan(0);

    unmount();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1500 * 5);
    });
    expect(getMineMock.mock.calls.length).toBe(callsBeforeUnmount);
  });

  // review finding F2: a second fallback-link click orphaned the first
  // poll's interval instead of settling it — finish() cleared whichever
  // interval id timerRef.current happened to hold (by then, the SECOND
  // poll's), so the first kept ticking, and unmount only ever cleared the
  // newest one. This repro drives connectFallback() twice, unmounts, then
  // proves zero more scmAccess.getMine() calls happen over the window the
  // bug used to leak through (the review's repro measured 10 over 15s).
  it("F2: a second connectFallback() call settles the first poll — no leaked interval after unmount", async () => {
    getMineMock.mockResolvedValue([]);

    const { result, unmount } = renderHook(() => useAdoConnect());
    act(() => {
      void result.current.connectFallback();
    });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1500);
    });
    const callsAfterFirstPollStarted = getMineMock.mock.calls.length;
    expect(callsAfterFirstPollStarted).toBeGreaterThan(0);

    // The fallback link clicked again — the first poll must not survive this.
    act(() => {
      void result.current.connectFallback();
    });

    unmount();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(15000);
    });

    expect(getMineMock.mock.calls.length).toBe(callsAfterFirstPollStarted);
  });

  // Review follow-up N6: the outstanding connect() promise resolves (false)
  // on unmount, rather than hanging forever with no one left to await it.
  it("N6: resolves the outstanding connect() promise on unmount, instead of leaving it pending", async () => {
    const popup = fakePopup();
    vi.spyOn(window, "open").mockReturnValue(popup as unknown as Window);
    getMineMock.mockResolvedValue([]); // never live — the popup stays "open" from this hook's view

    const { result, unmount } = renderHook(() => useAdoConnect());
    let resolved: boolean | null | "pending" = "pending";
    act(() => {
      void result.current.connect().then((ok) => {
        resolved = ok;
      });
    });
    expect(resolved).toBe("pending");

    unmount();
    await act(async () => {
      await Promise.resolve(); // let the resolved microtask settle
    });
    expect(resolved).toBe(false);
  });

  // Review regression finding 1 (PR #478): the earlier F2 fix settled a
  // superseded poll with `false` — which use-ado-launch-door.ts's settle()
  // reads as "the person declined", closing a dialog that should have
  // stayed open for the NEW attempt. A superseded poll is not a decline.
  it("regression 1: a superseded poll resolves null, not false", async () => {
    getMineMock.mockResolvedValue([]);

    const { result } = renderHook(() => useAdoConnect());
    let firstResolved: boolean | null | "pending" = "pending";
    act(() => {
      void result.current.connectFallback().then((ok) => {
        firstResolved = ok;
      });
    });

    // Supersede it before it can give up on its own.
    act(() => {
      void result.current.connectFallback();
    });
    await act(async () => {
      await Promise.resolve();
      await Promise.resolve();
    });

    expect(firstResolved).toBe(null);
  });

  // Review regression finding 2 (PR #478): cancel() must settle the
  // in-flight poll (as null — not a decline either) AND stop its interval,
  // so a dialog's Cancel can never let a late `true` toast onto a screen the
  // person has since moved to.
  it("regression 2: cancel() settles the in-flight poll as null and stops the interval", async () => {
    getMineMock.mockResolvedValue([]);

    const { result } = renderHook(() => useAdoConnect());
    let resolved: boolean | null | "pending" = "pending";
    act(() => {
      void result.current.connectFallback().then((ok) => {
        resolved = ok;
      });
    });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1500);
    });
    const callsBeforeCancel = getMineMock.mock.calls.length;
    expect(callsBeforeCancel).toBeGreaterThan(0);

    act(() => {
      result.current.cancel();
    });
    await act(async () => {
      await Promise.resolve();
    });
    expect(resolved).toBe(null);
    expect(result.current.connecting).toBe(false);

    await act(async () => {
      await vi.advanceTimersByTimeAsync(15000);
    });
    expect(getMineMock.mock.calls.length).toBe(callsBeforeCancel);
  });

  // Review regression finding 3 (PR #478): finish() was not idempotent for
  // `connecting` — a superseded poll's own straggling getMine() (already in
  // flight when it was superseded) could resolve "live" later and run
  // setConnecting(false) even though a NEWER poll was still ticking,
  // flipping the spinner/disabled state off under it.
  it("regression 3: a superseded poll's late straggler does not flip connecting off under a live newer poll", async () => {
    let resolveStale: ((rows: Array<{ state: string }>) => void) | undefined;
    const stale = new Promise<Array<{ state: string }>>((resolve) => {
      resolveStale = resolve;
    });
    getMineMock.mockReturnValueOnce(stale);
    getMineMock.mockResolvedValue([]); // every later call (the second poll) stays not-live

    const { result } = renderHook(() => useAdoConnect());
    act(() => {
      void result.current.connectFallback();
    });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1500);
    });
    expect(getMineMock).toHaveBeenCalledTimes(1); // the stale, still-pending call

    // Supersede before the stale getMine() settles.
    act(() => {
      void result.current.connectFallback();
    });
    expect(result.current.connecting).toBe(true);

    // The superseded poll's straggler now resolves "live" — must be a no-op.
    await act(async () => {
      resolveStale?.([{ state: "live" }]);
      await Promise.resolve();
      await Promise.resolve();
    });

    expect(result.current.connecting).toBe(true);
  });

  describe("connectFallback (review follow-up N1)", () => {
    it("never opens a popup", async () => {
      const openSpy = vi.spyOn(window, "open");
      getMineMock.mockResolvedValue([{ state: "live" }]);

      const { result } = renderHook(() => useAdoConnect());
      act(() => {
        void result.current.connectFallback();
      });
      await act(async () => {
        await vi.advanceTimersByTimeAsync(1500);
      });

      expect(openSpy).not.toHaveBeenCalled();
    });

    it("resolves true once a row reads live, the same poll connect() uses", async () => {
      getMineMock.mockResolvedValue([{ state: "live" }]);

      const { result } = renderHook(() => useAdoConnect());
      let resolved: boolean | null | undefined;
      act(() => {
        void result.current.connectFallback().then((ok) => {
          resolved = ok;
        });
      });
      expect(result.current.connecting).toBe(true);

      await act(async () => {
        await vi.advanceTimersByTimeAsync(1500);
      });

      expect(resolved).toBe(true);
      expect(result.current.connecting).toBe(false);
    });

    it("gives up and resolves false after the bounded timeout — there is no popup to watch", async () => {
      getMineMock.mockResolvedValue([]);

      const { result } = renderHook(() => useAdoConnect());
      let resolved: boolean | null | undefined;
      act(() => {
        void result.current.connectFallback().then((ok) => {
          resolved = ok;
        });
      });

      await act(async () => {
        await vi.advanceTimersByTimeAsync(5 * 60 * 1000 + 1500);
      });

      expect(resolved).toBe(false);
    });
  });
});
