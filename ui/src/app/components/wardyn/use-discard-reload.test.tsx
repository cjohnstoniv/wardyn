/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Discard re-reads /me before it reloads. A caller that closed while that read was out (an editor's Cancel, then
// another editor opened) must not have its load run: that load closes whatever is open now.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { act, renderHook } from "@testing-library/react";
import type { Me } from "../../lib/api/health";

const whoamiMock = vi.fn<() => Promise<Me | null>>();
vi.mock("../../lib/api/health", () => ({ health: { whoami: () => whoamiMock() } }));

import { useDiscardReload } from "./use-discard-reload";

const later = () => {
  let answer: (me: Me | null) => void = () => {};
  let fail: (e: Error) => void = () => {};
  const promise = new Promise<Me | null>((done, no) => {
    answer = done;
    fail = no;
  });
  return { promise, answer, fail };
};

beforeEach(() => whoamiMock.mockReset());

describe("useDiscardReload", () => {
  it("reloads a caller still open once /me answers, or fails", async () => {
    const load = vi.fn();
    const { result } = renderHook(() => useDiscardReload(load));
    whoamiMock.mockResolvedValueOnce(null).mockRejectedValueOnce(new Error("offline"));
    await act(async () => result.current());
    await act(async () => result.current());
    expect(load).toHaveBeenCalledTimes(2);
  });

  it("does not reload a caller that closed while /me was out, whether it answers or fails", async () => {
    const load = vi.fn();
    const answered = later();
    const failed = later();
    whoamiMock.mockReturnValueOnce(answered.promise).mockReturnValueOnce(failed.promise);
    const { result, unmount } = renderHook(() => useDiscardReload(load));
    result.current();
    result.current();
    unmount();
    await act(async () => {
      answered.answer(null);
      failed.fail(new Error("offline"));
    });
    expect(load).not.toHaveBeenCalled();
  });
});
