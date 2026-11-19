/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The source-control capability caps: the Go json tags on the boundary, the restriction and the dry run are the
// keys the console's TypeScript mirrors carry. The Go side is read from source.

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

describe("source-control capability caps wire", () => {
  it("azure_devops_capabilities and github_capabilities are the Go fields on the boundary and the restriction", () => {
    for (const file of ["internal/types/boundary.go", "internal/types/restriction.go"]) {
      const go = read(file);
      expect(go).toMatch(/AzureDevOpsCapabilities\s+\*\[\]adoscope\.Capability\s+`json:"azure_devops_capabilities,omitempty"`/);
      expect(go).toMatch(/GitHubCapabilities\s+\*\[\]ghscope\.Capability\s+`json:"github_capabilities,omitempty"`/);
    }
    for (const [file, name] of [
      ["ui/src/app/lib/api/boundary.ts", "BoundarySpec"],
      ["ui/src/app/lib/api/restrictions.ts", "RestrictionSpec"],
    ] as const) {
      const body = tsInterfaceBody(read(file), name);
      expect(body).toMatch(/azure_devops_capabilities\?: string\[\];/);
      expect(body).toMatch(/github_capabilities\?: string\[\];/);
    }
  });

  it("the dry run's provider_rows carry the Go tags", () => {
    const go = read("internal/api/boundary.go") + read("internal/api/vcs_capability_cap.go");
    expect(go).toMatch(/ProviderRows\s+\[\]boundaryProviderCap\s+`json:"provider_rows"`/);
    expect(go).toMatch(/CeilingRemoved\s+\[\]adoscope\.Capability\s+`json:"ceiling_removed"`/);
    expect(go).toMatch(/DefaultProfileRemoved\s+\[\]adoscope\.Capability\s+`json:"default_profile_removed,omitempty"`/);
    const ts = read("ui/src/app/lib/api/boundary.ts");
    expect(tsInterfaceBody(ts, "BoundaryReport")).toMatch(/provider_rows: BoundaryProviderCap\[\];/);
    const row = tsInterfaceBody(ts, "BoundaryProviderCap");
    expect(row).toMatch(/id: string;/);
    expect(row).toMatch(/ceiling_removed: string\[\];/);
    expect(row).toMatch(/default_profile_removed\?: string\[\];/);
  });
});
