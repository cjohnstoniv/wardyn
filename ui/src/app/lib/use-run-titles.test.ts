/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi } from "vitest";
import { renderHook, waitFor } from "@testing-library/react";
import { useRunTitles } from "./use-run-titles";

const listRunsMock = vi.fn();
vi.mock("./api/runs", () => ({ runs: { listRuns: () => listRunsMock() } }));

describe("useRunTitles (#3036)", () => {
  it("names an untitled run by the FIRST LINE of its task, and a titled one by its title", async () => {
    listRunsMock.mockResolvedValue([
      { id: "a", title: "", task: "Fix the login test\nthen update the docs\nand more" },
      { id: "b", title: "  Named run \nsecond line", task: "ignored" },
    ]);
    const { result } = renderHook(() => useRunTitles());
    await waitFor(() => expect(result.current.size).toBe(2));
    expect(result.current.get("a")).toBe("Fix the login test");
    expect(result.current.get("b")).toBe("Named run");
  });
});
