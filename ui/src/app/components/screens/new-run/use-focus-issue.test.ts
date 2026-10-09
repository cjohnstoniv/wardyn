/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it, vi } from "vitest";
import { renderHook } from "@testing-library/react";
import { rowDomId } from "./access-rows-model";
import { useFocusIssue } from "./use-focus-issue";

describe("useFocusIssue", () => {
  it("opens an Access row's row, then shows its panel at the row", () => {
    const order: string[] = [];
    const reveal = vi.fn(() => order.push("reveal"));
    const openRow = vi.fn(() => order.push("open"));
    const { result } = renderHook(() => useFocusIssue(reveal, openRow));
    const issue = { panel: "access" as const, focus: rowDomId("inline:0") };
    result.current(issue);
    expect(openRow).toHaveBeenCalledWith("inline:0");
    expect(reveal).toHaveBeenCalledWith(issue);
    expect(order).toEqual(["open", "reveal"]);
  });

  it("only reveals any other issue", () => {
    const reveal = vi.fn();
    const openRow = vi.fn();
    const { result } = renderHook(() => useFocusIssue(reveal, openRow));
    result.current({ panel: "run", focus: "nr-title" });
    expect(openRow).not.toHaveBeenCalled();
    expect(reveal).toHaveBeenCalledWith({ panel: "run", focus: "nr-title" });
  });
});
