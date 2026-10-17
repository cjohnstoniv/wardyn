/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { fieldSizeWords, sizeWords } from "./size-words";

describe("main drive size words (DR-0160)", () => {
  it("reads a stored size the way the field does: whole MiB under 0.1 GiB, else GiB with one decimal", () => {
    expect(fieldSizeWords(32)).toBe("32 MiB");
    expect(fieldSizeWords(102)).toBe("102 MiB");
    expect(fieldSizeWords(103)).toBe("0.1 GiB");
    expect(fieldSizeWords(4096)).toBe("4 GiB");
    expect(fieldSizeWords(1000, "floor")).toBe("0.9 GiB");
  });

  it("reads a cap or clamp exactly: GiB only when it is a whole tenth of a GiB", () => {
    expect(sizeWords(1000)).toBe("1000 MiB");
    expect(sizeWords(32)).toBe("32 MiB");
    expect(sizeWords(4096)).toBe("4 GiB");
    expect(sizeWords(1536)).toBe("1.5 GiB");
    expect(sizeWords(512)).toBe("0.5 GiB");
  });
});
