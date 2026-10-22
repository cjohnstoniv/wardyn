/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Go->TS wire parity for the egress refusal an `egress.deny` audit row carries
// (pkg/client.EgressRefusal, written by internal/egress.Refusal.AuditFields).
// The Go struct and the TS interface are read as text and diffed, so a key added
// on one side fails here rather than as an undefined at run time.
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
const clientGo = readFileSync(join(root, "pkg/client/egress_refusal.go"), "utf8");
const egressGo = readFileSync(join(root, "internal/egress/refusal.go"), "utf8");
const auditTs = readFileSync(join(root, "ui/src/app/lib/types/audit.ts"), "utf8");

function goJSONTags(src: string, structName: string): string[] {
  const m = new RegExp(`type\\s+${structName}\\s+struct\\s*\\{([\\s\\S]*?)\\n\\}`).exec(src);
  if (!m) throw new Error(`struct ${structName} not found`);
  return [...m[1].matchAll(/json:"([^",]+)(?:,[^"]*)?"/g)].map((t) => t[1]);
}

function tsInterfaceKeys(src: string, name: string): string[] {
  const m = new RegExp(`export\\s+interface\\s+${name}\\b[^{]*\\{([\\s\\S]*?)\\n\\}`).exec(src);
  if (!m) throw new Error(`interface ${name} not found`);
  const keys: string[] = [];
  for (const line of m[1].split("\n")) {
    const k = /^\s*([A-Za-z_][A-Za-z0-9_]*)\??\s*:/.exec(line);
    if (k) keys.push(k[1]);
  }
  return keys;
}

describe("egress refusal wire parity", () => {
  it("every Go EgressRefusal tag is on the TS interface, and nothing extra", () => {
    const goTags = goJSONTags(clientGo, "EgressRefusal");
    expect(goTags.length).toBe(4);
    expect(new Set(tsInterfaceKeys(auditTs, "EgressRefusal"))).toEqual(new Set(goTags));
  });

  it("the TS EgressRefusedBy union is the set of By values the Go side defines", () => {
    const goBy = [...egressGo.matchAll(/RefusedBy\w+\s*=\s*"([a-z]+)"/g)].map((m) => m[1]);
    const u = /export type EgressRefusedBy\s*=\s*([^;]+);/.exec(auditTs);
    if (!u) throw new Error("EgressRefusedBy not found");
    const tsBy = [...u[1].matchAll(/"([a-z]+)"/g)].map((m) => m[1]);
    expect(goBy.length).toBe(3);
    expect(new Set(tsBy)).toEqual(new Set(goBy));
  });
});
