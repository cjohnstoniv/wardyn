/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// mapRunMode keeps the older fields for a body the server would refuse beside carriers
// (carriersRefused in run-mode-map.ts). That mirrors two Go refusals in
// internal/api/run_mode.go; this reads the Go source by path, as wire-parity.test.ts does,
// so a field added to either refusal cannot go unmirrored.
import { existsSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { describe, expect, it } from "vitest";
import { mapRunMode, type RunModeBody } from "./run-mode-map";

function repoRoot(): string {
  let dir = resolve(process.cwd());
  for (let i = 0; i < 8; i++) {
    if (existsSync(join(dir, "go.mod"))) return dir;
    dir = dirname(dir);
  }
  throw new Error("go.mod not found walking up from " + process.cwd());
}

function goFunc(src: string, name: string): string {
  const m = new RegExp(`func ${name}\\([^)]*\\)[^{]*\\{([\\s\\S]*?)\\n\\}`).exec(src);
  if (!m) throw new Error(`func ${name} not found in run_mode.go`);
  return m[1];
}

const AGENT = { agent: "claude-code", task: "fix it", interactive: false } as const;
const body = (extra: Partial<RunModeBody>): RunModeBody => ({ ...AGENT, ...extra });
const policy = (p: Record<string, unknown>) => ({ inline_policy: p as unknown as RunModeBody["inline_policy"] });

describe("mapRunMode vs the server's run-mode refusals", () => {
  const src = readFileSync(join(repoRoot(), "internal/api/run_mode.go"), "utf8");

  it("the run-wide rules the server refuses are the ones the mirror refuses", () => {
    const wide = [...goFunc(src, "runModeRunWideRules").matchAll(/(?:at \+ "\.|return ")([a-z_.]+)"/g)].map((m) => m[1]);
    expect(wide).toEqual(["tool_rules", "push_rules", "overrides.agent.tool_rules"]);
    expect(mapRunMode(body({}))).not.toBeNull();
    expect(mapRunMode(body(policy({ tool_rules: [{ tool: "Bash", effect: "allow" }] })))).toBeNull();
    expect(mapRunMode(body(policy({ push_rules: { deny_paths: [".github/"] } })))).toBeNull();
    expect(mapRunMode(body({ overrides: { agent: { tool_rules: [{ tool: "Bash", effect: "allow" }] } } } as unknown as Partial<RunModeBody>))).toBeNull();
  });

  it("the background-only conflicts the server refuses are the ones the mirror refuses", () => {
    const fn = goFunc(src, "experienceConflictRefusal");
    const background = fn.slice(fn.indexOf("case client.ExperienceBackground:"), fn.indexOf("case client.ExperienceInteractive:"));
    expect([...background.matchAll(/pol\.(\w+)/g)].map((m) => m[1])).toEqual(["AutoStopAfterSec", "UIApps"]);
    expect(mapRunMode(body(policy({ auto_stop_after_sec: 600 })))).toBeNull();
    expect(mapRunMode(body(policy({ ui_apps: [{ name: "app", port: 8080 }] })))).toBeNull();
    // An interactive environment keeps its idle stop and its gateway.
    const interactive = { agent: "claude-code", task: "", interactive: true, interactive_start: "agent" } as const;
    expect(mapRunMode({ ...interactive, ...policy({ auto_stop_after_sec: 600 }) })).not.toBeNull();
  });
});
