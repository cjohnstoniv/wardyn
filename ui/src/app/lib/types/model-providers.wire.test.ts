/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The local_model wire parity: LocalModelSettings has exactly the json tags of the Go struct,
// and the defaults and limits the console shows are the ones the API enforces. The Go side is
// read from source.

import { existsSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { describe, expect, it } from "vitest";
import { LOCAL_MODEL_DEFAULTS, LOCAL_MODEL_LIMITS } from "./site";

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
  return [...m[1].matchAll(/json:"([^",]+)(?:,[^"]*)?"/g)].map((t) => t[1]);
}

function tsKeys(src: string, iface: string): string[] {
  const m = new RegExp(`export\\s+interface\\s+${iface}\\b[^{]*\\{([\\s\\S]*?)\\n\\}`).exec(stripComments(src));
  if (!m) throw new Error(`interface ${iface} not found`);
  return [...m[1].matchAll(/^\s*([a-z0-9_]+)\??\s*:/gm)].map((k) => k[1]);
}

const goType = read("internal/types/model_provider.go");
const goAPI = read("internal/api/model_providers.go");
const num = (src: string, name: string): number => {
  const m = new RegExp(`\\b${name}\\s*=\\s*([0-9 <*]+)`).exec(src);
  if (!m) throw new Error(`const ${name} not found`);
  return Function(`return ${m[1].trim()}`)() as number;
};

describe("local_model wire", () => {
  it("LocalModelSettings has full parity with its TS mirror", () => {
    const ts = read("ui/src/app/lib/types/site.ts");
    expect([...tsKeys(ts, "LocalModelSettings")].sort()).toEqual([...goJSONTags(goType, "LocalModelSettings")].sort());
  });

  it("ModelProvider carries the local block and the kind is in the closed set", () => {
    const ts = read("ui/src/app/lib/types/site.ts");
    expect(tsKeys(ts, "ModelProvider")).toContain("local");
    expect(goJSONTags(goType, "ModelProvider")).toContain("local");
    expect(goType).toContain('ModelProviderLocalModel ModelProviderKind = "local_model"');
    expect(ts).toContain('| "local_model"');
  });

  it("the defaults and limits are the API's", () => {
    expect(LOCAL_MODEL_DEFAULTS).toEqual({
      max_context: num(goType, "DefaultLocalMaxContext"),
      max_output: num(goType, "DefaultLocalMaxOutput"),
      keep_alive_seconds: num(goType, "DefaultLocalKeepAliveSeconds"),
    });
    expect(LOCAL_MODEL_LIMITS).toEqual({
      models: num(goAPI, "maxLocalModels"),
      bound: num(goAPI, "maxLocalBound"),
      keep_alive_seconds: num(goAPI, "maxLocalKeepAlive"),
    });
  });
});
