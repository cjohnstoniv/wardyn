/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { sizeWords } from "./size-words";

describe("main drive size words (DR-0160, STRINGS.md)", () => {
  it("reads GiB only when the size is a whole tenth of a GiB, else whole MiB, never rounded", () => {
    expect(sizeWords(1000)).toBe("1000 MiB");
    expect(sizeWords(5000)).toBe("5000 MiB");
    expect(sizeWords(32)).toBe("32 MiB");
    expect(sizeWords(102)).toBe("102 MiB");
    expect(sizeWords(103)).toBe("103 MiB");
    expect(sizeWords(4096)).toBe("4 GiB");
    expect(sizeWords(1536)).toBe("1.5 GiB");
    expect(sizeWords(512)).toBe("0.5 GiB");
  });
});
