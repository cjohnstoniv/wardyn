/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The credential delivery wire parity: each TS interface of types/credential-delivery.ts has exactly the
// json tags of the Go struct it mirrors, the closed sets are exactly the Go constants, and the bounds table
// is exactly classModes / fixedClasses. The Go side is read from source.

import { existsSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { describe, expect, it } from "vitest";
import { CLASS_MODES, DELIVERY_CLASSES, FIXED_CLASSES, type DeliveryMode } from "./credential-delivery";

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
  return [...m[1].matchAll(/json:"([^",]+)(?:,[^"]*)?"/g)].map((t) => t[1]).filter((t) => t !== "-");
}

function tsKeys(src: string, iface: string): string[] {
  const m = new RegExp(`export\\s+interface\\s+${iface}\\b[^{]*\\{([\\s\\S]*?)\\n\\}`).exec(stripComments(src));
  if (!m) throw new Error(`interface ${iface} not found`);
  return [...m[1].matchAll(/^\s*([a-z_]+)\??\s*:/gm)].map((k) => k[1]);
}

const ts = read("ui/src/app/lib/types/credential-delivery.ts");
const delivery = stripComments(read("internal/placement/delivery.go"));

// Go constant name -> its string value, for the Mode and Class constants of delivery.go.
const goConst = new Map([...delivery.matchAll(/^\s*((?:Mode|Class)\w+)\s+(?:Mode\s+)?= "([a-z_]+)"/gm)].map((m) => [m[1], m[2]]));
const goConstValue = (name: string): string => {
  const v = goConst.get(name);
  if (!v) throw new Error(`Go constant ${name} not found`);
  return v;
};

// classModes' entries: `ClassX: viaOrgResidentRefuse` (a named slice) or `ClassX: {ModeA, ModeB}`.
function goClassModes(): Record<string, string[]> {
  const block = /var classModes = map\[string\]\[\]Mode\{([\s\S]*?)\n\}/.exec(delivery);
  if (!block) throw new Error("classModes not found");
  const named = (id: string) => {
    const m = new RegExp(`var ${id} = \\[\\]Mode\\{([^}]*)\\}`).exec(delivery);
    if (!m) throw new Error(`slice ${id} not found`);
    return m[1].split(",").map((s) => goConstValue(s.trim())).filter(Boolean);
  };
  const out: Record<string, string[]> = {};
  for (const line of block[1].split("\n")) {
    const m = /^\s*(Class\w+):\s*(?:\{([^}]*)\}|(\w+))/.exec(line);
    if (!m) continue;
    out[goConstValue(m[1])] = m[2] !== undefined ? m[2].split(",").map((s) => goConstValue(s.trim())) : named(m[3]);
  }
  return out;
}

describe("credential delivery wire", () => {
  it.each([
    ["internal/placement/delivery.go", "PostureRequirement", "PostureRequirement"],
    ["internal/placement/delivery.go", "ClassPolicy", "ClassPolicy"],
    ["internal/placement/delivery.go", "DeliveryPolicy", "DeliveryPolicy"],
    ["internal/types/runner.go", "CredentialDeliveryPolicyDoc", "CredentialDeliveryPolicyDoc"],
  ])("%#: %s has full parity with its TS mirror", (file, goName, tsName) => {
    const tags = goJSONTags(read(file), goName);
    expect(tags.length).toBeGreaterThanOrEqual(1);
    expect(new Set(tsKeys(ts, tsName))).toEqual(new Set(tags));
  });

  it("DeliveryMode is the Go Mode constants", () => {
    const go = [...goConst].filter(([k]) => k.startsWith("Mode")).map(([, v]) => v);
    const m = /export type DeliveryMode =([^;]+);/.exec(stripComments(ts));
    if (!m) throw new Error("type DeliveryMode not found");
    expect(new Set([...m[1].matchAll(/"([a-z_]+)"/g)].map((x) => x[1]))).toEqual(new Set(go));
  });

  it("the class list is the Go Class constants", () => {
    const go = [...goConst].filter(([k]) => k.startsWith("Class")).map(([, v]) => v);
    expect(go.length).toBeGreaterThanOrEqual(14);
    expect(new Set(DELIVERY_CLASSES)).toEqual(new Set(go));
    expect(DELIVERY_CLASSES.length).toBe(go.length);
  });

  it("the bounds table is classModes, mode for mode and in the same order", () => {
    const go = goClassModes();
    expect(new Set(Object.keys(CLASS_MODES))).toEqual(new Set(Object.keys(go)));
    for (const [cls, modes] of Object.entries(go)) {
      expect([cls, CLASS_MODES[cls as keyof typeof CLASS_MODES]]).toEqual([cls, modes as DeliveryMode[]]);
    }
  });

  it("the fixed classes are fixedClasses", () => {
    const m = /var fixedClasses = \[\]string\{([^}]*)\}/.exec(delivery);
    if (!m) throw new Error("fixedClasses not found");
    expect(new Set(FIXED_CLASSES)).toEqual(new Set(m[1].split(",").map((s) => goConstValue(s.trim()))));
  });
});
