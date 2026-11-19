/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The real status client turns every failed /setup/status read into its unreachable fallback rather than
// throwing, so the hook must read that fallback as a failed read: "couldn't check", never the organisation's no.
import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, renderHook, waitFor } from "@testing-library/react";
import { useComputerFoldersAllowed } from "./computer-folder";

const reply = (status: number, body: unknown = {}) =>
  new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });
const status = (computer_folders: boolean) => ({ runner: { driver: "docker", confinement_classes: [], computer_folders } });

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("useComputerFoldersAllowed against the status client's fallback", () => {
  it("a server error is failed, not false; Try again reads again", async () => {
    const fetchMock = vi.fn().mockResolvedValueOnce(reply(500)).mockResolvedValue(reply(200, status(true)));
    vi.stubGlobal("fetch", fetchMock);
    const { result } = renderHook(() => useComputerFoldersAllowed());
    await waitFor(() => expect(result.current.failed).toBe(true));
    expect(result.current.allowed).toBeUndefined();
    act(() => result.current.retry());
    await waitFor(() => expect(result.current.allowed).toBe(true));
    expect(result.current.failed).toBe(false);
  });

  it("a dropped connection is failed", async () => {
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new TypeError("Failed to fetch")));
    const { result } = renderHook(() => useComputerFoldersAllowed());
    await waitFor(() => expect(result.current.failed).toBe(true));
    expect(result.current.allowed).toBeUndefined();
  });

  it("a read that says false is still a no", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(reply(200, status(false))));
    const { result } = renderHook(() => useComputerFoldersAllowed());
    await waitFor(() => expect(result.current.allowed).toBe(false));
    expect(result.current.failed).toBe(false);
  });
});
