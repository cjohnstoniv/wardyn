/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Go->TS wire parity for image requests (POST /governance/requests, kind image): the list's image_offered
// flag, the request kinds, and the image reasons. Read as text, as governance-requests.wire.test.ts does.
//
// Run: cd ui && pnpm exec vitest run src/app/lib/types/governance-image-request.wire.test.ts

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

// The json tags of one Go struct, skipping "-" and an embedded struct (no tag).
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

// The property names of one exported TS interface.
function tsInterfaceKeys(src: string, name: string): string[] {
  const m = new RegExp(`export\\s+interface\\s+${name}\\b[^{]*\\{([\\s\\S]*?)\\n\\}`).exec(stripComments(src));
  if (!m) throw new Error(`interface ${name} not found`);
  return [...m[1].matchAll(/([A-Za-z_][A-Za-z0-9_]*)\??\s*:/g)].map((k) => k[1]);
}

describe("Image requests wire (POST /governance/requests, kind image)", () => {
  const root = repoRoot();
  const api = readFileSync(join(root, "internal/api/governance_requests.go"), "utf8");
  const ts = readFileSync(join(root, "ui/src/app/lib/types/governance-requests.ts"), "utf8");

  it("the list's TypeScript keys are exactly its Go json tags, image_offered among them", () => {
    const goTags = goJSONTags(api, "governanceRequestList");
    expect(goTags).toContain("image_offered");
    expect(tsInterfaceKeys(ts, "GovernanceRequestList").sort()).toEqual(goTags.sort());
    expect(stripComments(ts)).toMatch(/image_offered: boolean;/);
  });

  it("the Go request kinds are exactly the GovernanceRequestKind union, image among them", () => {
    const table = /var governanceRequestKinds = map\[string\]governanceRequestKind\{([\s\S]*?)\n\}/.exec(api);
    if (!table) throw new Error("governanceRequestKinds not found");
    const goKinds = [...table[1].matchAll(/^\t"([a-z_]+)":/gm)].map((k) => k[1]).sort();
    expect(goKinds).toContain("image");
    const union = /export type GovernanceRequestKind = ([^;]+);/.exec(stripComments(ts));
    if (!union) throw new Error("GovernanceRequestKind not found");
    const tsKinds = [...union[1].matchAll(/"([a-z_]+)"/g)].map((k) => k[1]).sort();
    expect(tsKinds).toEqual(goKinds);
  });

  it("the image reasons are declared in reasons_governance.go", () => {
    const reasons = readFileSync(join(root, "internal/api/reasons_governance.go"), "utf8");
    for (const code of ["image_request_not_pinned", "image_request_not_offered", "image_already_granted"]) {
      expect(reasons, code).toContain(`"${code}"`);
    }
  });
});
