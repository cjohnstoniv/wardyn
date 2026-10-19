/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { TIER_PICKER } from "../../../lib/tier-picker-copy";
import { NO_BARRIER } from "./run-clone";

// #3050: one title states "this host has no barrier" on every surface, and the banner's second line
// depends on whether the reader can open setup.
describe("the no-barrier condition has one title", () => {
  it("the banner, the launch reason and the requirement card share it", () => {
    expect(NO_BARRIER.TITLE).toBe("This host can't run sandboxes yet — no barrier is installed.");
    expect(NO_BARRIER.BANNER_TITLE).toBe(NO_BARRIER.TITLE);
    expect(NO_BARRIER.LAUNCH_REASON).toBe(NO_BARRIER.TITLE);
    expect(TIER_PICKER.REQUIREMENT_TITLE).toBe(NO_BARRIER.TITLE);
  });

  it("someone who cannot open setup is told whom to ask, with no command", () => {
    expect(NO_BARRIER.BANNER_BODY_ASK).toBe("Ask a Wardyn admin to set one up.");
    expect(NO_BARRIER.BANNER_BODY_ASK).not.toMatch(/wardyn setup|`/);
  });

  it.each([
    "screens/runs-first-run.tsx",
    "screens/setup/environment-step.tsx",
  ])("%s takes the title from the canon, not its own wording", (file) => {
    const src = readFileSync(join(__dirname, "..", "..", file), "utf8");
    expect(src).toContain("NO_BARRIER.TITLE");
    expect(src).not.toMatch(/No sandbox (barrier|runner)/);
  });
});
