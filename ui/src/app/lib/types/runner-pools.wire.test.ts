/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The runner pool carriers' wire parity: each TS interface of types/runner-pools.ts
// has exactly the json tags of the Go struct it mirrors, and each closed value set
// is exactly the Go constants. The Go side is read from source.

import { existsSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { describe, expect, it } from "vitest";
import { RUNNER_POOL_DEFAULTS_PREF_KEY } from "./runner-pools";

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

const ts = read("ui/src/app/lib/types/runner-pools.ts");
const typesGo = read("internal/types/runner_pool.go");
const clientGo = read("pkg/client/runner_pools.go");

describe("runner pool carriers", () => {
  it.each([
    [typesGo, "RunnerPool", "RunnerPool"],
    [typesGo, "RunnerPoolMember", "RunnerPoolMember"],
    [typesGo, "RunnerPoolDefaults", "RunnerPoolDefaults"],
    [typesGo, "ResolvedRunnerPool", "ResolvedRunnerPool"],
    [typesGo, "RunnerPoolSubject", "RunnerPoolSubject"],
    [typesGo, "RunnerPoolUsePolicy", "RunnerPoolUsePolicy"],
    [clientGo, "RunnerPoolChoice", "RunnerPoolChoice"],
    [clientGo, "RunnerPoolList", "RunnerPoolList"],
    [clientGo, "CreateRunnerPoolRequest", "CreateRunnerPoolRequest"],
    [clientGo, "UpdateRunnerPoolRequest", "UpdateRunnerPoolRequest"],
  ])("%#: %s has full parity with its TS mirror", (go, goName, tsName) => {
    const tags = goJSONTags(go, goName);
    expect(tags.length).toBeGreaterThanOrEqual(1);
    expect(new Set(tsKeys(ts, tsName))).toEqual(new Set(tags));
  });

  it("the dry-run answers carry the pool facts under the Go names", () => {
    const run = read("ui/src/app/lib/types/runs.ts");
    const preview = read("ui/src/app/lib/types/policy-preview.ts");
    for (const [src, goSrc, goName] of [
      [run, read("pkg/client/runs_create.go"), "PreflightResult"],
      [preview, read("internal/api/policy_preview_facts.go"), "policyPreviewResponse"],
    ] as const) {
      const tags = goJSONTags(goSrc, goName).filter((t) => t.startsWith("runner_pool"));
      expect(tags.sort()).toEqual(["runner_pool", "runner_pools"]);
      for (const t of tags) expect(stripComments(src), t).toContain(`${t}?:`);
    }
    expect(goJSONTags(read("pkg/client/runs_create.go"), "CreateRunRequest")).toContain("runner_pool_id");
  });

  it("the closed value sets are the Go constants", () => {
    const union = (name: string) => {
      const m = new RegExp(`export type ${name} =([^;]+);`).exec(stripComments(ts));
      if (!m) throw new Error(`type ${name} not found`);
      return new Set([...m[1].matchAll(/"([a-z_]+)"/g)].map((x) => x[1]));
    };
    const consts = (src: string, pattern: string) => new Set([...src.matchAll(new RegExp(pattern, "g"))].map((m) => m[1]));
    expect(union("RunnerPoolHosting")).toEqual(consts(typesGo, '\\bRunnerPool\\w+\\s+RunnerPoolHosting = "([a-z_]+)"'));
    expect(union("RunnerPoolState")).toEqual(consts(typesGo, '\\bRunnerPool\\w+\\s+RunnerPoolState = "([a-z_]+)"'));
    expect(union("RunnerPoolSelection")).toEqual(consts(typesGo, '\\bRunnerPoolSelected\\w+\\s+RunnerPoolSelection = "([a-z_]+)"'));
    expect(union("RunnerPoolAvailability")).toEqual(consts(clientGo, '\\bRunnerPool(?:Available|Unavailable|Unknown)\\s*=\\s*"([a-z_]+)"'));
    expect(union("RunnerPoolHosting").size).toBe(2);
    expect(union("RunnerPoolState").size).toBe(3);
    expect(union("RunnerPoolSelection").size).toBe(3);
    expect(union("RunnerPoolAvailability").size).toBe(3);
  });

  it("the personal defaults key is the Go key", () => {
    expect(typesGo).toContain(`RunnerPoolDefaultsPrefKey = "${RUNNER_POOL_DEFAULTS_PREF_KEY}"`);
  });
});
