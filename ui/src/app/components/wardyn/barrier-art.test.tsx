/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { readdirSync, statSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";
import { render } from "@testing-library/react";
import { BarrierArt } from "./barrier-art";

// Vite inlines an asset below assetsInlineLimit (4096 bytes) as a data: URI,
// which the CSP blocks, so every shipped file must stay above it.
const ART_DIR = join(import.meta.dirname, "barrier-art");

describe("BarrierArt", () => {
  it("is a decorative sized image with a 128/256 srcset", () => {
    const { container } = render(<BarrierArt cc="CC1" size={88} />);
    const img = container.querySelector("img")!;
    expect(img).toHaveAttribute("alt", "");
    expect(img).toHaveAttribute("width", "88");
    expect(img).toHaveAttribute("height", "88");
    expect(img).toHaveAttribute("decoding", "async");
    expect(img.getAttribute("srcset")).toMatch(/ 128w, .* 256w$/);
    expect(img.src).not.toMatch(/^data:/);
  });

  it("ships exactly the six 128 and 256 px files, all too big to inline", () => {
    const files = readdirSync(ART_DIR).sort();
    expect(files).toEqual(
      ["fence", "vault", "wall"].flatMap((b) => [128, 256].map((s) => `barrier-${b}-${s}.png`)).sort(),
    );
    for (const f of files) expect(statSync(join(ART_DIR, f)).size).toBeGreaterThan(4096);
  });
});
