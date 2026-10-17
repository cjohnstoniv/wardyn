/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Every JSON tag of Go types.LimitsOverlay has a LIMIT_FIELDS row, so composing a
// standalone profile cannot silently drop a stored limit (seedOverlayLimits only
// carries the keys LIMIT_FIELDS names). Reads the Go source by path, as
// wire-parity.test.ts does.
import { existsSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { describe, expect, it } from "vitest";
import { LIMIT_FIELDS } from "./profile-overlay";

// Tags with no LIMIT_FIELDS row on purpose.
const EXEMPT: Record<string, string> = {
  // The rubric has its own section (profile-rubric.tsx), and seedOverlayLimits moves it explicitly.
  autonomy_rubric: "own Autonomy rubric section; seeded separately",
};

function repoRoot(): string {
  let dir = resolve(process.cwd());
  for (let i = 0; i < 8; i++) {
    if (existsSync(join(dir, "go.mod"))) return dir;
    dir = dirname(dir);
  }
  throw new Error("go.mod not found walking up from " + process.cwd());
}

describe("LIMIT_FIELDS vs Go types.LimitsOverlay", () => {
  it("has a row for every JSON tag unless explicitly exempt", () => {
    const src = readFileSync(join(repoRoot(), "internal/types/overlay.go"), "utf8");
    const body = /type\s+LimitsOverlay\s+struct\s*\{([\s\S]*?)\n\}/.exec(src);
    if (!body) throw new Error("struct LimitsOverlay not found");
    const tags = [...body[1].matchAll(/json:"([^",]+)(?:,[^"]*)?"/g)].map((m) => m[1]);
    expect(tags.length).toBeGreaterThan(10);
    const rows = new Set(LIMIT_FIELDS.map((f) => f.key));
    expect(tags.filter((t) => !rows.has(t) && !(t in EXEMPT))).toEqual([]);
    // An exemption that gained a row is stale.
    expect(Object.keys(EXEMPT).filter((t) => rows.has(t))).toEqual([]);
  });

  it("has no embedded struct, whose tags the scan above would not see", () => {
    expect(limitsOverlayLines().filter((line) => /^[A-Za-z_.*]+(\s+`[^`]*`)?$/.test(line))).toEqual([]);
  });

  it("types each row by the Go field: deny doors are true, permissive flags false, ints numbers, slices lists", () => {
    const want: Record<string, string> = {};
    for (const line of limitsOverlayLines()) {
      const m = /^\w+\s+(\S+)\s+`json:"([^",]+)/.exec(line);
      if (!m || m[2] in EXEMPT) continue;
      const [, goType, tag] = m;
      want[tag] = goType === "*bool" ? (tag.startsWith("deny_") ? "true" : "false") : goType === "*int" ? "number" : goType.startsWith("*[]") ? "list" : goType;
    }
    expect(Object.keys(want).length).toBeGreaterThan(10);
    const got = Object.fromEntries(LIMIT_FIELDS.filter((f) => f.key in want).map((f) => [f.key, f.kind]));
    expect(got).toEqual(want);
  });
});

// The trimmed field lines of Go types.LimitsOverlay.
function limitsOverlayLines(): string[] {
  const src = readFileSync(join(repoRoot(), "internal/types/overlay.go"), "utf8");
  const body = /type\s+LimitsOverlay\s+struct\s*\{([\s\S]*?)\n\}/.exec(src);
  if (!body) throw new Error("struct LimitsOverlay not found");
  return body[1].split("\n").map((l) => l.replace(/\s+/g, " ").trim()).filter((l) => l !== "" && !l.startsWith("//"));
}
