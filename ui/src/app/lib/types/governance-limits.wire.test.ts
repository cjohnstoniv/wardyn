/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The scheduled-runs door and the rubric's custom-component cap: the Go json tags on the structs the console
// edits are the keys its TypeScript mirrors carry. The Go side is read from source.

import { existsSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { describe, expect, it } from "vitest";

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

function tsInterfaceBody(src: string, name: string): string {
  const m = new RegExp(`export\\s+interface\\s+${name}\\b[^{]*\\{([\\s\\S]*?)\\n\\}`).exec(stripComments(src));
  if (!m) throw new Error(`interface ${name} not found`);
  return m[1];
}

describe("governance limits wire", () => {
  it("GovernanceLimits.deny_scheduled_runs and AutonomyRubric.self_defined_component are the Go fields", () => {
    const goGov = read("internal/types/governance.go");
    expect(goGov).toMatch(/DenyScheduledRuns\s+bool\s+`json:"deny_scheduled_runs,omitempty"`/);
    expect(goGov).toMatch(/SelfDefinedComponent\s+AutonomyLevel\s+`json:"self_defined_component,omitempty"`/);
    const govTs = read("ui/src/app/lib/api/governance.ts");
    expect(tsInterfaceBody(govTs, "GovernanceLimits")).toMatch(/deny_scheduled_runs\?: boolean;/);
    expect(tsInterfaceBody(govTs, "AutonomyRubric")).toMatch(/self_defined_component\?: AutonomyLevel;/);
    expect(tsInterfaceBody(read("ui/src/app/lib/api/restrictions.ts"), "RestrictionLimits")).toMatch(/deny_scheduled_runs\?: boolean;/);
  });
});
