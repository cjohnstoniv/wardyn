/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The narrow-only knobs a restriction carries: the Go json tags on RestrictionSpec and RestrictionLimits are the
// keys the TypeScript mirrors carry. The Go side is read from source.

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

function goStructBody(src: string, name: string): string {
  const m = new RegExp(`type\\s+${name}\\s+struct\\s*\\{([\\s\\S]*?)\\n\\}`).exec(stripComments(src));
  if (!m) throw new Error(`struct ${name} not found`);
  return m[1];
}

describe("restriction knobs wire", () => {
  it("RestrictionSpec carries the five policy knobs, in Go and in TypeScript", () => {
    const spec = goStructBody(read("internal/types/restriction.go"), "RestrictionSpec");
    expect(spec).toMatch(/AllowedDomains\s+\*\[\]string\s+`json:"allowed_domains,omitempty"`/);
    expect(spec).toMatch(/ToolRules\s+\*\[\]ToolRule\s+`json:"tool_rules,omitempty"`/);
    expect(spec).toMatch(/PushRules\s+\*PushRulesOverlay\s+`json:"push_rules,omitempty"`/);
    expect(spec).toMatch(/Desktop\s+\*DesktopControls\s+`json:"desktop,omitempty"`/);
    expect(spec).toMatch(/Recording\s+\*RecordingLevel\s+`json:"recording,omitempty"`/);
    const ts = tsInterfaceBody(read("ui/src/app/lib/api/restrictions.ts"), "RestrictionSpec");
    expect(ts).toMatch(/allowed_domains\?: string\[\];/);
    expect(ts).toMatch(/tool_rules\?: ToolRule\[\];/);
    expect(ts).toMatch(/push_rules\?: PushRulesSpec;/);
    expect(ts).toMatch(/desktop\?: DesktopControls;/);
    expect(ts).toMatch(/recording\?: RecordingLevel;/);
  });

  it("RestrictionLimits carries allowed_models, in Go and in TypeScript", () => {
    const limits = goStructBody(read("internal/types/restriction.go"), "RestrictionLimits");
    expect(limits).toMatch(/AllowedModels\s+\*\[\]string\s+`json:"allowed_models,omitempty"`/);
    const ts = tsInterfaceBody(read("ui/src/app/lib/api/restrictions.ts"), "RestrictionLimits");
    expect(ts).toMatch(/allowed_models\?: string\[\];/);
  });
});
