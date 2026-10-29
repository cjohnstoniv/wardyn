/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Wire parity for the workload mirrors: each TS interface of types/workloads.ts has exactly the json tags of
// the Go struct it mirrors, with the same keys optional, and each closed value set is the Go constants. The Go
// side is read from source.

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

const read = (p: string) => readFileSync(join(repoRoot(), p), "utf8");
const stripComments = (src: string) => src.replace(/\/\*[\s\S]*?\*\//g, "").replace(/\/\/.*$/gm, "");

function goBody(src: string, structName: string): string {
  const m = new RegExp(`type\\s+${structName}\\s+struct\\s*\\{([\\s\\S]*?)\\n\\}`).exec(src);
  if (!m) throw new Error(`struct ${structName} not found`);
  return m[1];
}
const goJSONTags = (src: string, name: string) =>
  [...goBody(src, name).matchAll(/json:"([^",]+)(?:,[^"]*)?"/g)].map((t) => t[1]).filter((t) => t !== "-");
const goOmitempty = (src: string, name: string) => [...goBody(src, name).matchAll(/json:"([^",]+),[^"]*omitempty[^"]*"/g)].map((t) => t[1]);

function tsBody(src: string, iface: string): string {
  const m = new RegExp(`export\\s+interface\\s+${iface}\\b[^{]*\\{([\\s\\S]*?)\\n\\}`).exec(stripComments(src));
  if (!m) throw new Error(`interface ${iface} not found`);
  return m[1];
}
const tsKeys = (src: string, iface: string) => [...tsBody(src, iface).matchAll(/^\s*([a-z0-9_]+)\??\s*:/gm)].map((k) => k[1]);
const tsOptional = (src: string, iface: string) => [...tsBody(src, iface).matchAll(/^\s*([a-z0-9_]+)\?\s*:/gm)].map((k) => k[1]);

function tsUnion(src: string, name: string): string[] {
  const m = new RegExp(`export\\s+type\\s+${name}\\s*=([^;]+);`).exec(stripComments(src));
  if (!m) throw new Error(`type ${name} not found`);
  return [...m[1].matchAll(/"([a-z_]+)"/g)].map((x) => x[1]);
}
const goConsts = (src: string, type: string) => [...src.matchAll(new RegExp(`\\w+\\s+${type}\\s*=\\s*"([a-z_]+)"`, "g"))].map((m) => m[1]);

const ts = read("ui/src/app/lib/types/workloads.ts");
const go = read("pkg/client/workloads.go");

describe("workloads wire", () => {
  it.each(["Workload", "WorkloadRefusal", "WorkloadEvent", "WorkloadLaunch", "WorkloadApplyResult", "WorkloadBinding", "CreateWorkloadBindingRequest"])(
    "%s has full parity with its Go struct",
    (name) => {
      expect(new Set(tsKeys(ts, name))).toEqual(new Set(goJSONTags(go, name)));
      expect(new Set(tsOptional(ts, name))).toEqual(new Set(goOmitempty(go, name)));
    },
  );

  it.each(["WorkloadSource", "WorkloadOutcome", "WorkloadLaunchState"])("%s is the Go constants", (name) => {
    expect(new Set(tsUnion(ts, name))).toEqual(new Set(goConsts(go, name)));
  });
});
