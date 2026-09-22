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

import { useAdoLaunchDoor } from "./use-ado-launch-door";
import { HttpError } from "../../../lib/api/core";

describe("useAdoLaunchDoor + useAdoConnect (F1)", () => {
  beforeEach(() => {
    getMineMock.mockReset();
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
});
