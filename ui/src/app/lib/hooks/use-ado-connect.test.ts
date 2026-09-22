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
  afterEach(() => vi.useRealTimers());

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
    let resolved: boolean | undefined;
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
    let resolved: boolean | undefined;
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

  it("a blocked popup (window.open returns null) sets blockedUrl and resolves false, with no polling", async () => {
    vi.spyOn(window, "open").mockReturnValue(null);

    const { result } = renderHook(() => useAdoConnect());
    let resolved: boolean | undefined;
    await act(async () => {
      resolved = await result.current.connect();
    });

    expect(resolved).toBe(false);
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
});
