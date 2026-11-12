/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The unit deny lists: the Go json tags on the limits, the overlay and the restriction are the keys the console's
// TypeScript mirrors carry. The Go side is read from source.

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

const root = repoRoot();
const read = (p: string) => readFileSync(join(root, p), "utf8");

const stripComments = (src: string) => src.replace(/\/\*[\s\S]*?\*\//g, "").replace(/\/\/.*$/gm, "");

function tsInterfaceBody(src: string, name: string): string {
  const m = new RegExp(`export\\s+interface\\s+${name}\\b[^{]*\\{([\\s\\S]*?)\\n\\}`).exec(stripComments(src));
  if (!m) throw new Error(`interface ${name} not found`);
  return m[1];
}

describe("unit deny lists wire", () => {
  it("deny_components and deny_model_providers are the Go fields on the limits, the overlay and the restriction", () => {
    for (const [file, ptr] of [
      ["internal/types/governance.go", ""],
      ["internal/types/overlay.go", "\\*"],
      ["internal/types/restriction.go", "\\*"],
    ] as const) {
      const go = read(file);
      expect(go).toMatch(new RegExp(`DenyComponents\\s+${ptr}\\[\\]string\\s+\`json:"deny_components,omitempty"\``));
      expect(go).toMatch(new RegExp(`DenyModelProviders\\s+${ptr}\\[\\]string\\s+\`json:"deny_model_providers,omitempty"\``));
    }
    for (const [file, name] of [
      ["ui/src/app/lib/api/governance.ts", "GovernanceLimits"],
      ["ui/src/app/lib/api/restrictions.ts", "RestrictionLimits"],
    ] as const) {
      const body = tsInterfaceBody(read(file), name);
      expect(body).toMatch(/deny_components\?: string\[\];/);
      expect(body).toMatch(/deny_model_providers\?: string\[\];/);
    }
  });
});
