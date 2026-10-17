/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// 0.8.6 profile composition: the stored row grew base_profile_id, overlay and overlay_limits, and the
// admin responses grew `effective`. Full parity between the Go structs and the TS mirror, read as text
// like runs.wire.fields.test.ts does: a Go rename of any of them would otherwise surface only as a
// runtime `undefined` in the editor.

import { describe, it, expect } from "vitest";
import { existsSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";

function repoRoot(): string {
  let dir = resolve(process.cwd());
  for (let i = 0; i < 8; i++) {
    if (existsSync(join(dir, "go.mod"))) return dir;
    dir = dirname(dir);
  }
  throw new Error("go.mod not found walking up from " + process.cwd());
}

function goJSONTags(src: string, structName: string): string[] {
  const m = new RegExp(`type\\s+${structName}\\s+struct\\s*\\{([\\s\\S]*?)\\n\\}`).exec(src);
  if (!m) throw new Error(`struct ${structName} not found`);
  const tags: string[] = [];
  for (const t of m[1].matchAll(/json:"([^",]+)(?:,[^"]*)?"/g)) {
    if (t[1] !== "-") tags.push(t[1]);
  }
  return tags;
}

function tsInterfaceKeys(src: string, name: string): string[] {
  const m = new RegExp(`export\\s+interface\\s+${name}\\b[^{]*\\{([\\s\\S]*?)\\n\\}`).exec(src);
  if (!m) throw new Error(`interface ${name} not found`);
  const keys: string[] = [];
  for (const line of m[1].split("\n")) {
    const k = /^\s*(?:readonly\s+)?([A-Za-z_][A-Za-z0-9_]*)\??\s*:/.exec(line);
    if (k) keys.push(k[1]);
  }
  return keys;
}

describe("source parity — composed governance profile wire types", () => {
  const root = repoRoot();
  const typesGo = readFileSync(join(root, "internal/types/governance.go"), "utf8");
  const writeGo = readFileSync(join(root, "internal/api/governance_profile_write.go"), "utf8");
  const governanceTs = readFileSync(join(root, "ui/src/app/lib/api/governance.ts"), "utf8");

  it("GovernanceProfile: every Go tag is mirrored, plus the view's own `effective`", () => {
    const goTags = goJSONTags(typesGo, "GovernanceProfile");
    expect(goTags).toEqual(expect.arrayContaining(["base_profile_id", "overlay", "overlay_limits"]));
    const view = goJSONTags(writeGo, "governanceProfileView");
    expect(view).toEqual(["effective"]);
    const tsKeys = tsInterfaceKeys(governanceTs, "GovernanceProfile");
    expect(new Set(tsKeys)).toEqual(new Set([...goTags, ...view]));
  });

  it("GovernanceEffective: full parity with governanceEffective", () => {
    const goTags = goJSONTags(writeGo, "governanceEffective");
    expect(goTags.length).toBe(4);
    const tsKeys = tsInterfaceKeys(governanceTs, "GovernanceEffective");
    expect(new Set(tsKeys)).toEqual(new Set(goTags));
  });
});
