/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Go->TS wire parity for unit_names on GET /governance: a unit admin's read sends the name of each unit above
// their subtrees, the client keeps it, and the Governance screen adds every listed unit's name to it rather than
// replacing it, so an envelope's root or a rule's unit above the subtree can be named.
//
// Run: cd ui && pnpm exec vitest run src/app/lib/api/governance-unit-names.wire.test.ts

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

describe("GET /governance unit_names", () => {
  const root = repoRoot();
  const go = readFileSync(join(root, "internal/api/governance.go"), "utf8");
  const client = readFileSync(join(root, "ui/src/app/lib/api/governance.ts"), "utf8");
  const screen = readFileSync(join(root, "ui/src/app/components/screens/governance/governance-screen.tsx"), "utf8");

  it("the server sends unit_names and the client keeps it", () => {
    expect(go).toMatch(/UnitNames\s+map\[uuid\.UUID\]string\s+`json:"unit_names,omitempty"`/);
    expect(client).toMatch(/^\s*unit_names\?: Record<string, string>;\n\s*\}>\(res\);/m);
    expect(client).toMatch(/^\s*unit_names: body\.unit_names,/m);
  });

  it("the screen adds the listed units' names to the server's", () => {
    expect(screen).toMatch(/unit_names: \{ \.\.\.s\.unit_names, \.\.\.Object\.fromEntries\(/);
  });
});
