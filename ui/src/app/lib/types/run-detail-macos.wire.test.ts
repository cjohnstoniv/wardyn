/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// GET /runs/{id}'s macos_preparing is its own field beside mac_window, which answers earlier rows (the site
// switch, the profile) before preparing. The Go side is read from source (internal/api/runs_policy.go).

import { existsSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { describe, expect, it } from "vitest";

function repoRoot(): string {
  let dir = resolve(process.cwd());
  for (let i = 0; i < 8; i++) {
    if (existsSync(join(dir, "go.mod"))) return dir;
    dir = dirname(dir);
  }
  throw new Error("go.mod not found");
}

const read = (p: string) => readFileSync(join(repoRoot(), p), "utf8");

function tsBody(src: string, iface: string): string {
  const m = new RegExp(`export\\s+interface\\s+${iface}\\b[^{]*\\{([\\s\\S]*?)\\n\\}`).exec(src);
  if (!m) throw new Error(`interface ${iface} not found`);
  return m[1];
}

describe("run detail macOS wire", () => {
  it("RunDetail carries macos_preparing, as GET /runs/{id} serves it", () => {
    expect(read("internal/api/runs_policy.go")).toMatch(/MacOSPreparing\s+bool\s+`json:"macos_preparing,omitempty"`/);
    expect(tsBody(read("ui/src/app/lib/types/runs.ts"), "RunDetail")).toMatch(/macos_preparing\?: boolean;/);
  });
});
