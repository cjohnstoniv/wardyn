/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Go->TS wire parity for the approval row's decider fields: decision_basis and decision_rule_unit on a DECIDED
// row, max_scope and second_human beside deciders on a PENDING row a rule governs. wire-parity.test.ts compares
// every ApprovalRequest key; this file pins these fields' Go types and the stored columns behind the decided pair.
//
// Run: cd ui && pnpm exec vitest run src/app/lib/types/approvals-basis.wire.test.ts

import { describe, it, expect } from "vitest";
import { existsSync, readFileSync, readdirSync } from "node:fs";
import { dirname, join, resolve } from "node:path";

function repoRoot(): string {
  let dir = resolve(process.cwd());
  for (let i = 0; i < 8; i++) {
    if (existsSync(join(dir, "go.mod"))) return dir;
    dir = dirname(dir);
  }
  throw new Error("go.mod not found walking up from " + process.cwd());
}

describe("Approval row decider fields", () => {
  const root = repoRoot();
  const typesGo = readFileSync(join(root, "internal/types/types.go"), "utf8");
  const storeGo = readFileSync(join(root, "internal/store/store.go"), "utf8");
  const ts = readFileSync(join(root, "ui/src/app/lib/types/approvals.ts"), "utf8");
  const migrations = readdirSync(join(root, "internal/db/migrations"));

  it("a decided row carries decision_basis and decision_rule_unit in Go and in TypeScript", () => {
    expect(typesGo).toMatch(/DecisionBasis\s+string\s+`json:"decision_basis,omitempty"`/);
    expect(typesGo).toMatch(/DecisionRuleUnit\s+\*uuid\.UUID\s+`json:"decision_rule_unit,omitempty"`/);
    expect(ts).toMatch(/^\s*decision_basis\?: string;/m);
    expect(ts).toMatch(/^\s*decision_rule_unit\?: string;/m);
  });

  it("the decided pair is stored on the approval row by its own migration", () => {
    expect(migrations.some((m) => /^\d{4}_approvals_decision_basis\.sql$/.test(m))).toBe(true);
    expect(storeGo).toMatch(/decision_basis=NULLIF\(\$\d+, ''\), decision_rule_unit=\$\d+/);
  });

  it("a pending row carries the rule's max_scope and second_human in Go and in TypeScript", () => {
    expect(typesGo).toMatch(/MaxScope\s+ApprovalScope\s+`json:"max_scope,omitempty"`/);
    expect(typesGo).toMatch(/SecondHuman\s+bool\s+`json:"second_human,omitempty"`/);
    expect(ts).toMatch(/^\s*max_scope\?: ApprovalScope;/m);
    expect(ts).toMatch(/^\s*second_human\?: boolean;/m);
  });
});
