/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// host_rules: the Go json tags of types.HostRuleSet and types.HostRule are the keys policy.ts mirrors,
// host_rules is on both RunPolicySpecs, and the effects the TypeScript union names are the ones the Go
// validation accepts today (hold is refused until it lands). The Go side is read from source.

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

function goJSONTags(src: string, structName: string): string[] {
  const m = new RegExp(`type\\s+${structName}\\s+struct\\s*\\{([\\s\\S]*?)\\n\\}`).exec(src);
  if (!m) throw new Error(`struct ${structName} not found`);
  return [...m[1].matchAll(/json:"([a-z_]+)[,"]/g)].map((x) => x[1]);
}

function tsInterfaceKeys(src: string, name: string): string[] {
  const m = new RegExp(`export\\s+interface\\s+${name}\\b[^{]*\\{([\\s\\S]*?)\\n\\}`).exec(stripComments(src));
  if (!m) throw new Error(`interface ${name} not found`);
  return [...m[1].matchAll(/^\s{2}([a-z_]+)\??:/gm)].map((x) => x[1]);
}

describe("host_rules wire", () => {
  const goSrc = read("internal/types/host_rules.go");
  const policyTs = read("ui/src/app/lib/types/policy.ts");

  it("HostRuleSet and HostRule: the Go tags are the TS keys", () => {
    expect(tsInterfaceKeys(policyTs, "HostRuleSet").sort()).toEqual(goJSONTags(goSrc, "HostRuleSet").sort());
    expect(tsInterfaceKeys(policyTs, "HostRule").sort()).toEqual(goJSONTags(goSrc, "HostRule").sort());
  });

  it("host_rules is on the Go and TypeScript RunPolicySpec", () => {
    expect(goJSONTags(read("internal/types/policy.go"), "RunPolicySpec")).toContain("host_rules");
    expect(tsInterfaceKeys(policyTs, "RunPolicySpec")).toContain("host_rules");
  });

  it("names the effects validation accepts today", () => {
    const union = /export type HostRuleEffect =([^;]+);/.exec(policyTs)?.[1] ?? "";
    expect(new Set([...union.matchAll(/"([a-z_]+)"/g)].map((m) => m[1]))).toEqual(new Set(["allow", "deny", "hold"]));
    expect(goSrc).toMatch(/case HostRuleAllow, HostRuleDeny, HostRuleHold:\n\t\treturn nil/);
  });
});
