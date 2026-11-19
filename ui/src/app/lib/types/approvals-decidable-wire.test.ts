/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Go->TS wire parity for GET /approvals?decidable_by=me: the two fields it adds to an approval row, and the
// query value and cap the Go handler accepts. wire-parity.test.ts already compares every ApprovalRequest key; this
// file pins the decidable list's own contract, and lives apart because that one is at the file-size cap.
//
// Run: cd ui && pnpm exec vitest run src/app/lib/types/approvals-decidable-wire.test.ts

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

describe("Approvals decidable list wire (GET /approvals?decidable_by=me)", () => {
  const root = repoRoot();
  const typesGo = readFileSync(join(root, "internal/types/types.go"), "utf8");
  const handler = readFileSync(join(root, "internal/api/approvals_decidable_by.go"), "utf8");
  const ts = readFileSync(join(root, "ui/src/app/lib/types/approvals.ts"), "utf8");

  it("an approval row carries requested_by in Go and in TypeScript", () => {
    expect(typesGo).toMatch(/RequestedBy\s+string\s+`json:"requested_by,omitempty"`/);
    expect(ts).toMatch(/^\s*requested_by\?: string;/m);
  });

  it("the handler accepts only decidable_by=me and caps the list at 500", () => {
    expect(handler).toMatch(/q\.Get\("decidable_by"\) != "me"/);
    expect(handler).toMatch(/maxDecidableByMe\s*=\s*500\b/);
  });
});
