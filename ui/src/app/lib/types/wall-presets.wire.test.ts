/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The wall preset wire parity: each TS interface of types/wall-presets.ts has exactly the json tags of the Go struct it
// mirrors, the scope union is the Go constants, and every reason the console branches on is a reason the server sends.
// The Go side is read from source.

import { existsSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { describe, expect, it } from "vitest";
import { WALL_PRESET_REASON } from "../api/wall-presets";
import { BUILTIN_PUSH_REFUSING } from "../wall-preset-words";

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

const ts = read("ui/src/app/lib/types/wall-presets.ts");

describe("wall presets wire", () => {
  it.each([
    ["internal/types/wall_preset.go", "WallPreset", "WallPreset"],
    ["pkg/client/wall_presets.go", "WallPresetList", "WallPresetList"],
    ["pkg/client/wall_presets.go", "WallPresetSaveRequest", "WallPresetSaveRequest"],
  ])("%#: %s has full parity with its TS mirror", (file, goName, tsName) => {
    const tags = goJSONTags(read(file), goName);
    expect(tags.length).toBeGreaterThanOrEqual(2);
    expect(new Set(tsKeys(ts, tsName))).toEqual(new Set(tags));
  });

  it("RunWallPreset (the run read) and the create field wall_preset are the client's", () => {
    expect(new Set(tsKeys(ts, "RunWallPreset"))).toEqual(new Set(goJSONTags(read("pkg/client/runs_create.go"), "RunWallPreset")));
    expect(goJSONTags(read("pkg/client/runs_create.go"), "CreateRunRequest")).toContain("wall_preset");
    expect(tsKeys(read("ui/src/app/lib/types/run-create.ts"), "CreateRunInput")).toContain("wall_preset");
  });

  it("WallPresetScope is the Go constants", () => {
    const go = [...read("internal/types/wall_preset.go").matchAll(/WallPresetScope[A-Za-z]+\s+WallPresetScope = "([a-z]+)"/g)].map((m) => m[1]);
    const m = /export type WallPresetScope =([^;]+);/.exec(stripComments(ts));
    if (!m) throw new Error("type WallPresetScope not found");
    expect(go.length).toBe(4);
    expect(new Set([...m[1].matchAll(/"([a-z]+)"/g)].map((x) => x[1]))).toEqual(new Set(go));
  });

  it("the built-ins that refuse every push are the ones the Go built-ins build from readOnlyWalls", () => {
    const go = read("internal/types/wall_preset.go");
    expect(/func readOnlyWalls\(\)[\s\S]*?DenyAll:/.test(go)).toBe(true);
    // Read-only is readOnlyWalls itself and Sealed is a copy of it that only narrows further; Open is empty.
    expect(/readOnly := readOnlyWalls\(\)\s+sealed := readOnlyWalls\(\)/.test(go)).toBe(true);
    expect([...BUILTIN_PUSH_REFUSING].sort()).toEqual(["read-only", "sealed"]);
  });

  it("every reason the console branches on is one the server sends", () => {
    const sent = new Set([...read("internal/api/reasons.go").matchAll(/=\s*"(wall_preset[a-z_]*)"/g)].map((m) => m[1]));
    for (const reason of Object.values(WALL_PRESET_REASON)) expect(sent, reason).toContain(reason);
  });
});
