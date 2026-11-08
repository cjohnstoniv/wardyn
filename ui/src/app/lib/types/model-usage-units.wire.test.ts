/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Go->TS wire parity for GET /governance/units/{id}/model-usage. The Go structs and the TS interfaces are read
// as text and diffed, so a field added on one side fails here rather than as an undefined at run time.
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

const root = repoRoot();
const unitsGo = readFileSync(join(root, "internal/api/governance_unit_usage.go"), "utf8");
const usageTs = readFileSync(join(root, "ui/src/app/lib/types/model-usage.ts"), "utf8");

// The json tags of one Go struct body; an embedded struct carries no tag of its own.
function goJSONTags(src: string, structName: string): string[] {
  const m = new RegExp(`type\\s+${structName}\\s+struct\\s*\\{([\\s\\S]*?)\\n\\}`).exec(src);
  if (!m) throw new Error(`struct ${structName} not found`);
  return [...m[1].matchAll(/json:"([^",]+)(?:,[^"]*)?"/g)].map((t) => t[1]);
}

function tsInterfaceKeys(src: string, name: string): string[] {
  const m = new RegExp(`export\\s+interface\\s+${name}\\b[^{]*\\{([\\s\\S]*?)\\n\\}`).exec(src.replace(/\/\*[\s\S]*?\*\//g, "").replace(/\/\/.*$/gm, ""));
  if (!m) throw new Error(`interface ${name} not found`);
  const keys: string[] = [];
  for (const line of m[1].split("\n")) {
    const k = /^\s*([A-Za-z_][A-Za-z0-9_]*)\??\s*:/.exec(line);
    if (k) keys.push(k[1]);
  }
  return keys;
}

describe("unit model usage wire parity", () => {
  it("UnitModelUsageResponse has every Go tag and nothing extra", () => {
    const goTags = goJSONTags(unitsGo, "UnitModelUsageResponse");
    expect(goTags).toEqual(["since", "until", "units"]);
    expect(new Set(tsInterfaceKeys(usageTs, "UnitModelUsageResponse"))).toEqual(new Set(goTags));
    expect(usageTs).toMatch(/units:\s*UnitModelUsage\[\]/);
  });

  it("UnitModelUsage is the Go struct's own tags, and both embed the single-run usage view", () => {
    const own = goJSONTags(unitsGo, "UnitModelUsageView");
    expect(own).toEqual(["unit_id", "unit_name"]);
    expect(new Set(tsInterfaceKeys(usageTs, "UnitModelUsage"))).toEqual(new Set(own));
    expect(usageTs).toMatch(/interface UnitModelUsage extends ModelUsage\b/);
    expect(unitsGo).toMatch(/type\s+UnitModelUsageView\s+struct\s*\{[^}]*\n\s*ModelUsageView\b/);
  });
});
