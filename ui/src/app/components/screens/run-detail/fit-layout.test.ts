/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { fitRows, settleItems } from "./fit-layout";

describe("fitRows (#3038)", () => {
  it("counts the rows a block needs, gaps included", () => {
    // 2 rows of 32px with a 10px gap are 74px tall.
    expect(fitRows(74, 32, 10)).toBe(2);
    expect(fitRows(75, 32, 10)).toBe(3);
    expect(fitRows(1, 32, 10)).toBe(1);
  });
});

describe("settleItems (#3038)", () => {
  const item = (i: string, x: number, y: number, w: number, h: number) => ({ i, x, y, w, h, minH: 2 });

  it("resizes the measured tiles, keeps the pinned one, and drops what is below up under what shares its columns", () => {
    const out = settleItems(
      [item("terminal", 0, 0, 6, 8), item("a", 6, 0, 6, 4), item("b", 6, 4, 6, 4), item("c", 0, 8, 6, 4)],
      { terminal: 3, a: 2, b: 5 },
      ["terminal"],
    );
    const by = Object.fromEntries(out.map((it) => [it.i, it]));
    expect(by.terminal).toMatchObject({ y: 0, h: 8 });
    expect(by.a).toMatchObject({ y: 0, h: 2 });
    expect(by.b).toMatchObject({ y: 2, h: 5 });
    expect(by.c).toMatchObject({ y: 8, h: 4 });
  });

  it("never goes under a tile's minimum rows", () => {
    expect(settleItems([item("a", 0, 0, 6, 4)], { a: 1 }, [])[0].h).toBe(2);
  });
});
