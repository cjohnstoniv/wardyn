/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, expect, it } from "vitest";
import { CC_META } from "./cc-meta";

// The server picks the default barrier (the org default, else the strongest
// offered class), so no tier's static copy may claim to be the default.
describe("cc-meta — no tier claims to be the default", () => {
  it.each(["CC1", "CC2", "CC3"] as const)("%s tagline and mechanism do not say default", (cc) => {
    expect(CC_META[cc].tagline).not.toMatch(/default/i);
    expect(CC_META[cc].mechanism).not.toMatch(/default/i);
  });

  it("new-run-canon.md quotes the Wall mechanism line", () => {
    const doc = readFileSync(resolve(process.cwd(), "../docs/design/new-run-canon.md"), "utf8");
    expect(doc).toContain(CC_META.CC2.mechanism);
  });
});
