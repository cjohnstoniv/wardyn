/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The context pins: the held change kind, the two pin requests and the org component view's pin keys
// are the Go wire the console reads. The Go side is read from source.

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

function goStructBody(src: string, name: string): string {
  const m = new RegExp(`type\\s+${name}\\s+struct\\s*\\{([\\s\\S]*?)\\n\\}`).exec(src);
  if (!m) throw new Error(`struct ${name} not found`);
  return m[1];
}

describe("context pins wire", () => {
  it("the component pin's held kind is a Go target with a console label", () => {
    expect(read("internal/types/governance_change.go")).toMatch(
      /GovernanceTargetComponentOwningUnit\s*=\s*"component_owning_unit"/,
    );
    expect(read("ui/src/app/lib/governance-copy.ts")).toMatch(/\bcomponent_owning_unit:\s*"/);
  });

  it("the component pin request takes unit_id", () => {
    const body = goStructBody(read("internal/api/component_owning_unit.go"), "componentOwningUnitRequest");
    expect(body).toMatch(/UnitID\s+\*uuid\.UUID\s+`json:"unit_id"`/);
  });

  it("the workspace pin request takes an optional strict switch", () => {
    const body = goStructBody(read("internal/api/workspace_owning_unit.go"), "owningUnitRequest");
    expect(body).toMatch(/Strict\s+\*bool\s+`json:"strict,omitempty"`/);
  });

  it("the org component view carries the pin", () => {
    const body = goStructBody(read("pkg/client/components.go"), "OrgComponentView");
    expect(body).toContain('`json:"owning_unit_id,omitempty"`');
    expect(body).toContain('`json:"owning_unit_name,omitempty"`');
  });
});
