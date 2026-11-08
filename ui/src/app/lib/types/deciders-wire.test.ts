/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Go->TS wire parity for the unit deciders DTOs (GET /governance deciders, PUT /governance/units/{id}/deciders):
// the Go json tags and the TS interface keys are read as text and compared, as wire-parity.test.ts does for
// the other DTOs. Kept in its own file because that one is at the file-size cap.
//
// Run: cd ui && pnpm exec vitest run src/app/lib/types/deciders-wire.test.ts

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

function stripComments(src: string): string {
  return src.replace(/\/\*[\s\S]*?\*\//g, "").replace(/\/\/.*$/gm, "");
}

// The property names of one flat exported TS interface.
function tsInterfaceKeys(src: string, name: string): string[] {
  const m = new RegExp(`export\\s+interface\\s+${name}\\b[^{]*\\{([\\s\\S]*?)\\n\\}`).exec(stripComments(src));
  if (!m) throw new Error(`interface ${name} not found`);
  return [...m[1].matchAll(/^\s*([A-Za-z_][A-Za-z0-9_]*)\??\s*:/gm)].map((k) => k[1]);
}

describe("Unit deciders wire (GET /governance deciders, PUT /governance/units/{id}/deciders)", () => {
  const root = repoRoot();
  const types = readFileSync(join(root, "internal/types/deciders.go"), "utf8");
  const api = readFileSync(join(root, "internal/api/governance_deciders.go"), "utf8");
  const ts = readFileSync(join(root, "ui/src/app/lib/api/deciders.ts"), "utf8");

  it("each deciders DTO's TypeScript keys are exactly its Go json tags", () => {
    const pairs: [string, string, string][] = [
      [types, "DeciderRule", "DeciderRule"],
      [types, "UnitDeciders", "UnitDeciders"],
      [api, "governanceDecidersView", "GovernanceDeciders"],
    ];
    for (const [go, goName, tsName] of pairs) {
      expect(tsInterfaceKeys(ts, tsName).sort(), tsName).toEqual(goJSONTags(go, goName).sort());
    }
  });

  it("DeciderKind is the Go closed set of kinds", () => {
    const goKinds = [...types.matchAll(/\bDeciderKind\w+\s*=\s*"([a-z_]+)"/g)].map((m) => m[1]);
    const union = /export type DeciderKind =([^;]+);/.exec(stripComments(ts))?.[1] ?? "";
    const tsKinds = [...union.matchAll(/"([a-z_]+)"/g)].map((m) => m[1]);
    expect(goKinds.length).toBe(9);
    expect(tsKinds.sort()).toEqual(goKinds.sort());
  });

  it("GET /governance carries the deciders field and a lock names the deciders knob", () => {
    const gov = readFileSync(join(root, "internal/api/governance.go"), "utf8");
    expect(gov).toMatch(/Deciders\s+\[\]governanceDecidersView\s+`json:"deciders,omitempty"`/);
    const govTs = readFileSync(join(root, "ui/src/app/lib/api/governance.ts"), "utf8");
    expect(stripComments(govTs)).toMatch(/deciders\?: GovernanceDeciders\[\];/);
    expect(types).toMatch(/DeciderLockKnob\s*=\s*"deciders"/);
  });
});
