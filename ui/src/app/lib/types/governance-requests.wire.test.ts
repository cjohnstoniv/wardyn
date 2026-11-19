/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Go->TS wire parity for held requests (GET/POST /governance/requests): the Go json tags and the TS
// interface keys are read as text and compared, as deciders-wire.test.ts does for the deciders DTOs.
//
// Run: cd ui && pnpm exec vitest run src/app/lib/types/governance-requests.wire.test.ts

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

describe("Governance requests wire (GET/POST /governance/requests)", () => {
  const root = repoRoot();
  const api = readFileSync(join(root, "internal/api/governance_requests.go"), "utf8");
  const ts = readFileSync(join(root, "ui/src/app/lib/types/governance-requests.ts"), "utf8");

  it("each request DTO's TypeScript keys are exactly its Go json tags", () => {
    const pairs: [string, string][] = [
      ["GovernanceRequestView", "GovernanceRequest"],
      ["ElevationOffer", "ElevationOffer"],
      ["governanceRequestBody", "GovernanceRequestBody"],
    ];
    for (const [goName, tsName] of pairs) {
      expect(tsInterfaceKeys(ts, tsName).sort(), tsName).toEqual(goJSONTags(api, goName).sort());
    }
  });

  it("the request view embeds the held change", () => {
    expect(api).toMatch(/type GovernanceRequestView struct \{\n\ttypes\.GovernanceChange\n/);
    expect(stripComments(ts)).toMatch(/export interface GovernanceRequest extends GovernanceChange \{/);
  });

  it("a membership's expires_at is an omitempty Go tag and an optional TS key", () => {
    const gov = readFileSync(join(root, "internal/types/governance.go"), "utf8");
    expect(gov).toMatch(/ExpiresAt \*time\.Time `json:"expires_at,omitempty"`/);
    const govTs = readFileSync(join(root, "ui/src/app/lib/api/governance.ts"), "utf8");
    expect(tsInterfaceKeys(govTs, "GovernanceMembership")).toContain("expires_at");
    expect(stripComments(govTs)).toMatch(/expires_at\?: string;/);
  });

  it("a run's lifetime_cap_source tag is mirrored with its three sources", () => {
    const typesGo = readFileSync(join(root, "internal/types/types.go"), "utf8");
    expect(typesGo).toMatch(/LifetimeCapSource LimitSource `json:"lifetime_cap_source,omitempty"`/);
    const runs = readFileSync(join(root, "ui/src/app/lib/types/runs.ts"), "utf8");
    expect(stripComments(runs)).toMatch(/lifetime_cap_source\?: "pool" \| "deployment" \| "elevation";/);
  });

  it("an approver kind can be a group", () => {
    const change = readFileSync(join(root, "ui/src/app/lib/types/governance-change.ts"), "utf8");
    expect(stripComments(change)).toMatch(/kind: "security" \| "unit_admin" \| "group";/);
  });
});
