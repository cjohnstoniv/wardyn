/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Go->TS wire parity for the platform envelope: the Go struct and the TS interface are read as text and
// diffed, GET /governance's `envelopes` field is read by the client, and the held kind has a console name.
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
const read = (p: string) => readFileSync(join(root, p), "utf8");
const envelopeGo = read("internal/types/envelope.go");
const changeGo = read("internal/types/governance_change.go");
const governanceGo = read("internal/api/governance.go");
const envelopeTs = read("ui/src/app/lib/types/governance-envelope.ts");
const apiTs = read("ui/src/app/lib/api/governance.ts");
const copyTs = read("ui/src/app/lib/governance-copy.ts");

function goJSONTags(src: string, structName: string): string[] {
  const m = new RegExp(`type\\s+${structName}\\s+struct\\s*\\{([\\s\\S]*?)\\n\\}`).exec(src);
  if (!m) throw new Error(`struct ${structName} not found`);
  return [...m[1].matchAll(/json:"([^",]+)(?:,[^"]*)?"/g)].map((t) => t[1]);
}

function tsInterfaceKeys(src: string, name: string): string[] {
  const m = new RegExp(`export\\s+interface\\s+${name}\\b[^{]*\\{([\\s\\S]*?)\\n\\}`).exec(src.replace(/\/\*[\s\S]*?\*\//g, "").replace(/\/\/.*$/gm, ""));
  if (!m) throw new Error(`interface ${name} not found`);
  return m[1].split("\n").flatMap((line) => {
    const k = /^\s*([A-Za-z_][A-Za-z0-9_]*)\??\s*:/.exec(line);
    return k ? [k[1]] : [];
  });
}

describe("platform envelope wire parity", () => {
  it("GovernanceEnvelope has every Go tag and nothing extra", () => {
    const goTags = goJSONTags(envelopeGo, "GovernanceEnvelope");
    expect(goTags).toEqual(["unit_id", "approved_groups", "set_by", "set_at"]);
    expect(new Set(tsInterfaceKeys(envelopeTs, "GovernanceEnvelope"))).toEqual(new Set(goTags));
  });

  it("GET /governance carries envelopes and the client reads them", () => {
    expect(goJSONTags(governanceGo, "governanceResponse")).toContain("envelopes");
    expect(apiTs).toMatch(/body\.envelopes/);
    expect(apiTs).toMatch(/envelopes\?: GovernanceEnvelope\[\]/);
  });

  it("the held kind has a console name", () => {
    const kind = /GovernanceTargetEnvelope\s*=\s*"([a-z_]+)"/.exec(changeGo);
    expect(kind).not.toBeNull();
    expect(copyTs).toContain(`${kind![1]}: "Platform envelope"`);
  });
});
