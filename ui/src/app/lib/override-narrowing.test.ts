/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The narrowing table's parity: the console's copy is the Go table's JSON, and
// its TS unions are exactly the Go constants. Run:
//   cd ui && pnpm vitest run src/app/lib/override-narrowing.test.ts

import { existsSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { describe, expect, it } from "vitest";
import {
  hostOverrideKind,
  OVERRIDE_NARROWING,
  overrideMayBeClamped,
  overrideOffered,
  overrideRule,
  type OverrideKind,
  type OverrideOp,
} from "./override-narrowing";

function repoRoot(): string {
  let dir = resolve(process.cwd());
  for (let i = 0; i < 8; i++) {
    if (existsSync(join(dir, "go.mod"))) return dir;
    dir = dirname(dir);
  }
  throw new Error("go.mod not found");
}

const goSrc = readFileSync(join(repoRoot(), "internal/types/override_narrowing.go"), "utf8");
const consts = (type: string) => [...goSrc.matchAll(new RegExp(`\\b\\w+\\s+${type} = "([a-z_]+)"`, "g"))].map((m) => m[1]);

describe("override narrowing table", () => {
  it("its kinds, operations, directions and rules are the Go constants", () => {
    const kinds = new Set(consts("OverrideKind"));
    const ops = new Set(consts("OverrideOp"));
    const directions = new Set(consts("OverrideDirection"));
    const rules = new Set(consts("OverrideRule"));
    expect(kinds.size).toBeGreaterThanOrEqual(9);
    expect(new Set(OVERRIDE_NARROWING.map((r) => r.kind))).toEqual(kinds);
    for (const r of OVERRIDE_NARROWING) {
      expect(ops.has(r.op)).toBe(true);
      expect(directions.has(r.direction)).toBe(true);
      expect(rules.has(r.rule)).toBe(true);
    }
  });

  it("holds OD-1's rule: tightening is allowed, widening never plainly", () => {
    for (const r of OVERRIDE_NARROWING) {
      if (r.direction === "tighten") expect(r.rule).toBe("allowed");
      else expect(r.rule).not.toBe("allowed");
    }
  });

  it("answers per operation", () => {
    expect(overrideOffered("agent_host", "add")).toBe(true);
    expect(overrideMayBeClamped("agent_host", "add")).toBe(true);
    expect(overrideMayBeClamped("agent_host", "remove")).toBe(false);
    expect(overrideOffered("agent_host_wildcard", "add")).toBe(false);
    expect(overrideOffered("tool_rule_allow", "add")).toBe(false);
    expect(overrideOffered("tool_rule_restrict", "add")).toBe(true);
    expect(overrideOffered("push_rule_deny", "add")).toBe(true);
    expect(overrideOffered("push_rule_deny", "remove")).toBe(false);
    expect(overrideOffered("ado_capability", "add")).toBe(false);
    expect(overrideOffered("ado_capability", "narrow")).toBe(true);
    // A pair with no row is never offered.
    expect(overrideRule("agent_host", "narrow")).toBeUndefined();
    expect(overrideOffered("agent_host", "narrow")).toBe(false);
  });

  it("classifies a wildcard host as its own, refused, kind", () => {
    const kind: OverrideKind = hostOverrideKind("*.example.com");
    const op: OverrideOp = "add";
    expect(kind).toBe("agent_host_wildcard");
    expect(overrideOffered(kind, op)).toBe(false);
    expect(hostOverrideKind("api.example.com")).toBe("agent_host");
  });
});
