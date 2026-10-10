/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The runners management wire parity: each TS interface of types/runners.ts has exactly the json
// tags of the Go struct it mirrors, and each closed value set is exactly the Go constants. The Go
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
  return [...m[1].matchAll(/^\s*([a-z_]+)\??\s*:/gm)].map((k) => k[1]);
}

const ts = read("ui/src/app/lib/types/runners.ts");

describe("runners management wire", () => {
  it.each([
    ["internal/types/runner_inventory.go", "RunnerView", "RunnerView"],
    ["internal/types/runner_inventory.go", "RunnerSettingsRequest", "RunnerSettingsRequest"],
    ["internal/types/runner_registration.go", "RunnerRegistrationToken", "RunnerRegistrationToken"],
    ["internal/types/runner.go", "RunnerPosture", "RunnerPosture"],
  ])("%#: %s has full parity with its TS mirror", (file, goName, tsName) => {
    const tags = goJSONTags(read(file), goName);
    expect(tags.length).toBeGreaterThanOrEqual(1);
    expect(new Set(tsKeys(ts, tsName))).toEqual(new Set(tags));
  });

  it("RunnerState is the Go constants", () => {
    const go = [...read("internal/types/runner.go").matchAll(/Runner(?:Unclaimed|Claimed|Revoked)\s+RunnerState = "([a-z]+)"/g)].map((m) => m[1]);
    const m = /export type RunnerState =([^;]+);/.exec(stripComments(ts));
    if (!m) throw new Error("type RunnerState not found");
    expect(new Set([...m[1].matchAll(/"([a-z]+)"/g)].map((x) => x[1]))).toEqual(new Set(go));
  });

  it("the state filter is what the handler accepts", () => {
    const src = read("internal/api/runner_inventory.go");
    const m = /export type RunnerFilter =([^;]+);/.exec(ts);
    if (!m) throw new Error("type RunnerFilter not found");
    for (const f of [...m[1].matchAll(/"([a-z]+)"/g)].map((x) => x[1])) expect(src).toContain(`"${f}"`);
  });
});
