/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A held kubectl write's scope: the TS KubeCallScope in types/approvals.ts has exactly the json tags of
// internal/types.KubeCallScope. The Go side is read from source.

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

function goJSONTags(src: string, structName: string): string[] {
  const m = new RegExp(`type\\s+${structName}\\s+struct\\s*\\{([\\s\\S]*?)\\n\\}`).exec(src);
  if (!m) throw new Error(`struct ${structName} not found`);
  return [...m[1].matchAll(/json:"([^",]+)(?:,[^"]*)?"/g)].map((t) => t[1]).filter((t) => t !== "-");
}

const stripComments = (src: string) => src.replace(/\/\*[\s\S]*?\*\//g, "").replace(/\/\/.*$/gm, "");

function tsKeys(src: string, iface: string): string[] {
  const m = new RegExp(`export\\s+interface\\s+${iface}\\b[^{]*\\{([\\s\\S]*?)\\n\\}`).exec(stripComments(src));
  if (!m) throw new Error(`interface ${iface} not found`);
  return [...m[1].matchAll(/^\s*([a-z_0-9]+)\??\s*:/gm)].map((k) => k[1]);
}

describe("KubeCallScope wire parity", () => {
  it("has exactly the Go struct's keys", () => {
    const goTags = goJSONTags(read("internal/types/kube_call.go"), "KubeCallScope");
    expect(goTags.length).toBeGreaterThanOrEqual(8); // stale-regex guard
    expect(new Set(tsKeys(read("ui/src/app/lib/types/approvals.ts"), "KubeCallScope"))).toEqual(new Set(goTags));
  });
});
