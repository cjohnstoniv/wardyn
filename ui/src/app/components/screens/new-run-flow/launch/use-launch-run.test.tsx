/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it, vi } from "vitest";
import { act, renderHook } from "@testing-library/react";
import { getAuthGeneration } from "../../../../lib/api/core";
import { runs } from "../../../../lib/api/runs";
import { useLaunchRun } from "./use-launch-run";

afterEach(() => vi.restoreAllMocks());

const identity = () => ({ principal: "alice", resolved: true, revision: 1, authGeneration: getAuthGeneration() });
const BODY = { title: "x" } as never;

async function launched(session?: string) {
  const create = vi.spyOn(runs, "createRunWire").mockResolvedValue({ id: "run-1" } as never);
  const { result } = renderHook(() => useLaunchRun(identity(), vi.fn(), vi.fn()));
  await act(async () => {
    await result.current.launch(BODY, session);
  });
  return create;
}

describe("useLaunchRun and the assistant's session", () => {
  it("sends the session's header when the draft came from one", async () => {
    const create = await launched("s-1");
    expect(create).toHaveBeenCalledWith(BODY, { "X-Wardyn-Assistant-Session": "s-1" });
  });

  it("sends no header for a session that is only whitespace", async () => {
    const create = await launched("   ");
    expect(create.mock.calls[0]).toEqual([BODY]);
  });

  it("sends the request exactly as before when there is no session", async () => {
    const create = await launched();
    expect(create).toHaveBeenCalledTimes(1);
    expect(create.mock.calls[0]).toEqual([BODY]);
  });
});
