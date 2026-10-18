/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The run containment wire parity: each TS interface of types/run-containment.ts has exactly the json
// tags of the Go struct it mirrors, and each closed value set is exactly the Go constants. The Go side is
// read from source.

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
  return [...m[1].matchAll(/^\s*([a-z0-9_]+)\??\s*:/gm)].map((k) => k[1]);
}

function tsUnion(src: string, name: string): Set<string> {
  const m = new RegExp(`export type ${name} =([^;]+);`).exec(stripComments(src));
  if (!m) throw new Error(`type ${name} not found`);
  return new Set([...m[1].matchAll(/"([a-z_]+)"/g)].map((x) => x[1]));
}

const ts = read("ui/src/app/lib/types/run-containment.ts");
const goTypes = read("internal/types/run_containment.go");

describe("run containment wire", () => {
  it.each([
    ["internal/types/run_containment.go", "RunContainment"],
    ["internal/types/run_containment.go", "RunContainmentPending"],
    ["internal/types/run_containment.go", "RunNodeRecord"],
    ["internal/types/run_containment.go", "RunNodeRecordRequest"],
    ["internal/runner/runner.go", "NodeTuple"],
    ["internal/runner/runner.go", "NodeTuplePod"],
    ["internal/runner/runner.go", "NodeSample"],
  ])("%#: %s %s has full parity with its TS mirror", (file, name) => {
    const tags = goJSONTags(read(file), name);
    expect(tags.length).toBeGreaterThanOrEqual(1);
    expect(new Set(tsKeys(ts, name))).toEqual(new Set(tags));
  });

  it.each([
    ["ContainmentPendingKind", /ContainmentPending(?:AgentOnly|Delete)\s+= "([a-z_]+)"/g],
    ["NodeRecordAttestation", /NodeRecord(?:PoweredOff|NoPriorHolder)\s+= "([a-z_]+)"/g],
    ["NodeRecordSource", /NodeRecordSource\w+\s+= "([a-z_]+)"/g],
    ["NodeRecordState", /NodeRecord(?:Sent|Accepted|Refused)\s+= "([a-z_]+)"/g],
  ])("%s is the Go constants", (name, re) => {
    const go = [...goTypes.matchAll(re as RegExp)].map((m) => m[1]);
    expect(go.length).toBeGreaterThanOrEqual(2);
    expect(tsUnion(ts, name)).toEqual(new Set(go));
  });

  it("RunContainmentReason is the Go reason block", () => {
    const block = /Kubernetes runner node-loss containment[^\n]*\n[^\n]*\nconst \(([\s\S]*?)\n\)/.exec(read("internal/api/reasons.go"));
    if (!block) throw new Error("containment reason block not found");
    const go = [...block[1].matchAll(/= "([a-z_]+)"/g)].map((m) => m[1]);
    expect(go.length).toBeGreaterThanOrEqual(5);
    expect(tsUnion(ts, "RunContainmentReason")).toEqual(new Set(go));
  });
});
