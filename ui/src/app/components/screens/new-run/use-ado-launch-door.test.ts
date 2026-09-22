/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Review finding F1: the launch door and useAdoConnect exercised TOGETHER
// through a real blocked popup — new-run-rail.test.tsx only ever injected
// `blockedUrl` into an already-open dialog by hand, and
// new-run-screen.test.tsx mocked useAdoConnect outright with
// `blockedUrl: null`, so nothing proved the two actually cooperate when the
// browser refuses window.open().
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { renderHook, act } from "@testing-library/react";

const getMineMock = vi.fn();
vi.mock("../../../lib/api/scm-access", () => ({
  scmAccess: { getMine: (...a: unknown[]) => getMineMock(...a) },
}));

const toastSuccessMock = vi.fn();
vi.mock("sonner", () => ({ toast: { success: (...a: unknown[]) => toastSuccessMock(...a) } }));

import { useAdoLaunchDoor } from "./use-ado-launch-door";
import { HttpError } from "../../../lib/api/core";

describe("useAdoLaunchDoor + useAdoConnect (F1)", () => {
  beforeEach(() => {
    getMineMock.mockReset();
    toastSuccessMock.mockReset();
  });
  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("a real blocked popup keeps the dialog open and surfaces the fallback link, instead of closing it", async () => {
    // The browser refuses window.open() outright — the actual condition
    // useAdoConnect's connect() checks, not a hand-set blockedUrl prop.
    vi.spyOn(window, "open").mockReturnValue(null);

    const { result } = renderHook(() => useAdoLaunchDoor());

    act(() => {
      result.current.notifyLaunchError(
        new HttpError(422, "not connected", "git_credential", "https://dev.azure.com/contoso"),
      );
    });
    expect(result.current.dialog.open).toBe(true);
    expect(result.current.dialog.blockedUrl).toBe(null);

    await act(async () => {
      result.current.dialog.onConfirm();
      await Promise.resolve();
      await Promise.resolve();
    });

    // The bug: settle(false) ran and closed the dialog, so the fallback
    // link the blockedUrl below feeds never had a chance to render.
    expect(result.current.dialog.open).toBe(true);
    expect(result.current.dialog.blockedUrl).toBe("/api/v1/scm/azure-devops/signin");
    expect(getMineMock).not.toHaveBeenCalled();
  });

  it("a real, un-blocked connect that the person cancels still closes the dialog", async () => {
    const popup = { closed: false, close: vi.fn(), opener: {} as unknown, location: { href: "" } };
    vi.spyOn(window, "open").mockReturnValue(popup as unknown as Window);
    vi.useFakeTimers();
    getMineMock.mockResolvedValue([]);

    const { result } = renderHook(() => useAdoLaunchDoor());
    act(() => {
      result.current.notifyLaunchError(new HttpError(422, "not connected", "git_credential", "contoso"));
    });

    act(() => {
      result.current.dialog.onConfirm();
    });
    popup.closed = true;
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1500);
    });

    expect(result.current.dialog.open).toBe(false);
    vi.useRealTimers();
  });

  // Review regression finding 1 (PR #478): the F2 fix settled a superseded
  // poll with `false`, which settle() reads as "the person declined" and
  // closes the dialog — so on exactly the path this PR is about (a blocked
  // popup, the fallback link clicked, Connect pressed again) the dialog
  // vanished while the fresh popup was still open. Reproduced through the
  // real hook + real door, not a hand-set prop.
  it("regression 1: a fresh Connect attempt superseding the fallback's poll does not close the dialog", async () => {
    const openSpy = vi.spyOn(window, "open").mockReturnValueOnce(null);
    getMineMock.mockResolvedValue([]);

    const { result } = renderHook(() => useAdoLaunchDoor());
    act(() => {
      result.current.notifyLaunchError(
        new HttpError(422, "not connected", "git_credential", "https://dev.azure.com/contoso"),
      );
    });

    // window.open is blocked: onConfirm sets blockedUrl and leaves the
    // dialog open (F1, pinned above).
    await act(async () => {
      result.current.dialog.onConfirm();
      await Promise.resolve();
    });
    expect(result.current.dialog.blockedUrl).toBe("/api/v1/scm/azure-devops/signin");

    // The fallback link is clicked — its bounded poll starts.
    act(() => {
      result.current.dialog.onFallbackClick();
    });
    expect(result.current.dialog.open).toBe(true);

    // Connect is pressed again — this time window.open succeeds, and the
    // new popup+poll supersedes the fallback poll still in flight.
    const popup = { closed: false, close: vi.fn(), opener: {} as unknown, location: { href: "" } };
    openSpy.mockReturnValueOnce(popup as unknown as Window);
    await act(async () => {
      result.current.dialog.onConfirm();
      await Promise.resolve();
    });

    // The regression: the superseded fallback poll resolved false, settle
    // ran, and setOpen(false) fired — closing the dialog while the new
    // popup is still open on screen.
    expect(result.current.dialog.open).toBe(true);
  });

  // Review regression finding 2 (PR #478): Cancel only closed the dialog —
  // nothing stopped the in-flight poll, so it ran on for the full
  // FALLBACK_POLL_TIMEOUT_MS, and a late `true` could fire the relaunch
  // toast on whatever screen the person had since moved to.
  it("regression 2: Cancel stops the poll — no further scmAccess calls, no stray relaunch toast", async () => {
    vi.useFakeTimers();
    vi.spyOn(window, "open").mockReturnValue(null);
    getMineMock.mockResolvedValue([]);

    const { result } = renderHook(() => useAdoLaunchDoor());
    act(() => {
      result.current.notifyLaunchError(new HttpError(422, "not connected", "git_credential", "contoso"));
    });
    act(() => {
      result.current.dialog.onConfirm();
    });
    act(() => {
      result.current.dialog.onFallbackClick();
    });

    await act(async () => {
      await vi.advanceTimersByTimeAsync(1500);
    });
    const callsBeforeCancel = getMineMock.mock.calls.length;
    expect(callsBeforeCancel).toBeGreaterThan(0);

    act(() => {
      result.current.dialog.onCancel();
    });
    expect(result.current.dialog.open).toBe(false);

    // A row goes "live" from here on — if the poll were still running, it
    // would pick this up and settle(true) would toast.
    getMineMock.mockResolvedValue([{ state: "live" }]);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(15000);
    });

    expect(getMineMock.mock.calls.length).toBe(callsBeforeCancel);
    expect(toastSuccessMock).not.toHaveBeenCalled();
    vi.useRealTimers();
  });
});
