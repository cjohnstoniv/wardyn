/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Go->TS wire parity for a held change's review and its impact: the Go json tags of the server's structs
// (internal/api/governance_change_review.go, governance_change_impact.go and what they embed) against the TS
// interfaces and the SDK's structs. Same technique as governance-change-note.wire.test.ts (it reads both
// sources as text), kept standalone so each file runs on its own.
//
// The approvals block and the approval counts have no Go struct to read yet, so they have no parity test here.
//
// Run: cd ui && pnpm exec vitest run src/app/lib/types/change-review.wire.test.ts

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

function read(rel: string): string {
  return readFileSync(join(repoRoot(), rel), "utf8");
}

function goJSONTags(src: string, structName: string): string[] {
  const m = new RegExp(`type\\s+${structName}\\s+struct\\s*\\{([\\s\\S]*?)\\n\\}`).exec(src);
  if (!m) throw new Error(`struct ${structName} not found`);
  const tags: string[] = [];
  for (const t of m[1].matchAll(/json:"([^",]+)(?:,[^"]*)?"/g)) {
    if (t[1] !== "-") tags.push(t[1]);
  }
  return tags.sort();
}

function stripComments(src: string): string {
  return src.replace(/\/\*[\s\S]*?\*\//g, "").replace(/\/\/.*$/gm, "");
}

// The top-level property names of one exported TS interface.
function tsInterfaceTopKeys(src: string, name: string): string[] {
  const m = new RegExp(`export\\s+interface\\s+${name}\\b[^{]*\\{([\\s\\S]*?)\\n\\}`).exec(stripComments(src));
  if (!m) throw new Error(`interface ${name} not found`);
  const keys: string[] = [];
  let depth = 0;
  let start = 0;
  const body = m[1];
  for (let i = 0; i <= body.length; i++) {
    const ch = body[i];
    if (ch === "{") depth++;
    else if (ch === "}") depth--;
    else if (depth === 0 && (i === body.length || ch === ";" || ch === "\n")) {
      const k = /^\s*(?:readonly\s+)?([A-Za-z_][A-Za-z0-9_]*)\??\s*:/.exec(body.slice(start, i));
      if (k) keys.push(k[1]);
      start = i + 1;
    }
  }
  return keys.sort();
}

// One Go struct, its SDK twin and its TS interface carry the same keys.
function expectParity(goSrc: string, goName: string, sdkName: string, tsSrc: string, tsName: string): void {
  const go = goJSONTags(goSrc, goName);
  expect(go.length, `${goName} read no tags`).toBeGreaterThan(0);
  expect(goJSONTags(read("pkg/client/governance_review.go"), sdkName), `SDK ${sdkName} against ${goName}`).toEqual(go);
  expect(tsInterfaceTopKeys(tsSrc, tsName), `TS ${tsName} against ${goName}`).toEqual(go);
}

describe("Change review and impact wire (diff.review, GET /governance/changes/{id}/impact)", () => {
  const review = read("internal/api/governance_change_review.go");
  const impact = read("internal/api/governance_change_impact.go");
  const ts = read("ui/src/app/lib/types/governance-change.ts");

  it("the review keeps the Go, SDK and TypeScript keys in parity", () => {
    expectParity(review, "changeReview", "GovernanceChangeReview", ts, "ChangeReview");
    expectParity(review, "riskGrade", "GovernanceRiskGrade", ts, "ChangeRiskSide");
    expectParity(read("internal/composer/leq_explain.go"), "Widening", "GovernanceChangeWidening", ts, "ChangeWidening");
    expectParity(read("internal/composer/risk.go"), "RiskItem", "GovernanceRiskItem", read("ui/src/app/lib/types/runs.ts"), "RiskItem");
    // The risk pair is an inline object on the TS review.
    expect(goJSONTags(review, "changeRisk")).toEqual(["after", "before"]);
    expect(goJSONTags(read("pkg/client/governance_review.go"), "GovernanceChangeRisk")).toEqual(["after", "before"]);
    expect(/risk\?:\s*\{\s*before:[^;]*;\s*after:/.test(stripComments(ts))).toBe(true);
  });

  it("the impact keeps the Go, SDK and TypeScript keys in parity", () => {
    expectParity(impact, "changeImpact", "GovernanceChangeImpact", ts, "ChangeImpact");
    expectParity(impact, "changeImpactRun", "GovernanceImpactRun", ts, "ChangeImpactRun");
    // people is the tree dry run's person; boundary is the boundary dry run's report.
    const tsGov = read("ui/src/app/lib/api/governance.ts");
    expectParity(read("internal/api/governance_dry_run.go"), "treeDryRunPerson", "GovernanceImpactPerson", tsGov, "TreeDryRunPerson");
    const goBoundary = read("internal/api/boundary.go");
    const tsBoundary = read("ui/src/app/lib/api/boundary.ts");
    expectParity(goBoundary, "boundaryReport", "GovernanceImpactBoundary", tsBoundary, "BoundaryReport");
    expectParity(goBoundary, "boundaryReportRow", "GovernanceImpactRow", tsBoundary, "BoundaryReportRow");
    expectParity(goBoundary, "boundaryReportAssign", "GovernanceImpactAssign", tsBoundary, "BoundaryReportAssign");
    expectParity(read("internal/api/boundary_meet.go"), "boundaryChange", "GovernanceImpactChange", tsBoundary, "BoundaryChange");
    expectParity(read("internal/api/vcs_capability_cap.go"), "boundaryProviderCap", "GovernanceImpactCap", tsBoundary, "BoundaryProviderCap");
    // activates and vacates are the unit refs of a legacy membership move.
    expect(goJSONTags(read("internal/api/governance_memberships.go"), "unitActivation")).toEqual(goJSONTags(read("pkg/client/governance_review.go"), "GovernanceUnitRef"));
  });

  it("the review and the impact travel on the SDK change and the TS diff", () => {
    const sdk = read("pkg/client/governance.go");
    expect(goJSONTags(sdk, "GovernanceChangeDiff")).toContain("review");
    expect(tsInterfaceTopKeys(ts, "GovernanceChangeDiff")).toContain("review");
    expect(goJSONTags(read("internal/api/governance_change_gate.go"), "governanceDiffBody")).toContain("review");
  });
});
