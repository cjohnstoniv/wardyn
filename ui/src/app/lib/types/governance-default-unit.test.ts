/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// DEFAULT_GOVERNANCE_UNIT_ID is the Go DefaultUnitID, read as text so the two cannot drift.
// Run: cd ui && pnpm vitest run src/app/lib/types/governance-default-unit.test.ts

import { describe, it, expect } from "vitest";
import { existsSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { DEFAULT_GOVERNANCE_UNIT_ID } from "./governance";

function repoRoot(): string {
  let dir = resolve(__dirname);
  while (!existsSync(join(dir, "go.mod"))) {
    const up = dirname(dir);
    if (up === dir) throw new Error("go.mod not found above " + __dirname);
    dir = up;
  }
  return dir;
}

describe("DEFAULT_GOVERNANCE_UNIT_ID", () => {
  it("is store.DefaultUnitID", () => {
    const src = readFileSync(join(repoRoot(), "internal/store/units.go"), "utf8");
    const m = /var DefaultUnitID = uuid\.MustParse\("([0-9a-f-]{36})"\)/.exec(src);
    expect(m, "found no DefaultUnitID in internal/store/units.go").not.toBeNull();
    expect(DEFAULT_GOVERNANCE_UNIT_ID).toBe(m![1]);
  });
});
