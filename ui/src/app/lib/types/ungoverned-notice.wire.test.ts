/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The policy preview's `ungoverned` block: types/policy-preview.ts has exactly the json tags of
// pkg/client's UngovernedNotice. The Go side is read from source.

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

describe("ungoverned notice wire", () => {
  it("UngovernedNotice has exactly the Go struct's json tags", () => {
    const go = /type\s+UngovernedNotice\s+struct\s*\{([\s\S]*?)\n\}/.exec(stripComments(read("pkg/client/runs_ungoverned.go")))?.[1] ?? "";
    const goTags = [...go.matchAll(/json:"([^",]+)/g)].map((m) => m[1]);
    const tsBody = /export\s+interface\s+UngovernedNotice\b[^{]*\{([\s\S]*?)\n\}/.exec(stripComments(read("ui/src/app/lib/types/policy-preview.ts")))?.[1] ?? "";
    const tsKeys = [...tsBody.matchAll(/^\s*([a-z_]+)\??\s*:/gm)].map((k) => k[1]);
    expect(goTags).toHaveLength(6);
    expect(new Set(tsKeys)).toEqual(new Set(goTags));
  });
});
