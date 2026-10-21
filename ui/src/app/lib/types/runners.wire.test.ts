/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The runners management wire parity: each TS interface of types/runners.ts has exactly the json
// tags of the Go struct it mirrors, and each closed value set is exactly the Go constants. The Go
// side is read from source.

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

function goJSONTags(src: string, structName: string): string[] {
  const m = new RegExp(`type\\s+${structName}\\s+struct\\s*\\{([\\s\\S]*?)\\n\\}`).exec(src);
  if (!m) throw new Error(`struct ${structName} not found`);
  return [...m[1].matchAll(/json:"([^",]+)(?:,[^"]*)?"/g)].map((t) => t[1]).filter((t) => t !== "-");
}

const stripComments = (src: string) => src.replace(/\/\*[\s\S]*?\*\//g, "").replace(/\/\/.*$/gm, "");

function tsKeys(src: string, iface: string): string[] {
  const m = new RegExp(`export\\s+interface\\s+${iface}\\b[^{]*\\{([\\s\\S]*?)\\n\\}`).exec(stripComments(src));
  if (!m) throw new Error(`interface ${iface} not found`);
  return [...m[1].matchAll(/^\s*([a-z0-9_]+)\??\s*:/gm)].map((k) => k[1]);
}

const ts = read("ui/src/app/lib/types/runners.ts");

describe("runners management wire", () => {
  it.each([
    ["internal/types/runner_inventory.go", "RunnerView", "RunnerView"],
    ["internal/types/runner_inventory.go", "RunnerSettingsRequest", "RunnerSettingsRequest"],
    ["internal/types/runner_registration.go", "RunnerRegistrationToken", "RunnerRegistrationToken"],
    ["internal/types/runner.go", "RunnerPosture", "RunnerPosture"],
    ["internal/types/runner.go", "RunnerFailure", "RunnerFailure"],
    ["internal/types/runner_inventory.go", "RunnerCapacity", "RunnerCapacity"],
    ["internal/types/runner_limits.go", "RunnerAmounts", "RunnerAmounts"],
    ["internal/types/runner_limits.go", "RunnerLimits", "RunnerLimits"],
    ["internal/types/runner_limits.go", "RunnerTally", "RunnerTally"],
    ["internal/types/runner_limits.go", "RunnerUsage", "RunnerUsage"],
    ["internal/types/runner_inventory.go", "RunnerUpdateRequest", "RunnerUpdateRequest"],
    ["internal/types/runner_registration_wire.go", "RunnerTokenRequest", "RunnerTokenRequest"],
    ["internal/types/runner_registration_wire.go", "RunnerClaimRequest", "RunnerClaimRequest"],
    ["internal/types/runner_registration_wire.go", "RunnerRegistration", "RunnerRegistration"],
    ["internal/types/runner_registration_wire.go", "RunnerOrgTokenRequest", "RunnerOrgTokenRequest"],
    ["internal/types/runner_registration_wire.go", "RunnerTrustRequest", "RunnerTrustRequest"],
  ])("%#: %s has full parity with its TS mirror", (file, goName, tsName) => {
    const tags = goJSONTags(read(file), goName);
    expect(tags.length).toBeGreaterThanOrEqual(1);
    expect(new Set(tsKeys(ts, tsName))).toEqual(new Set(tags));
  });

  it("RunnerState is the Go constants", () => {
    const go = [...read("internal/types/runner.go").matchAll(/Runner(?:Unclaimed|Claimed|Revoked)\s+RunnerState = "([a-z]+)"/g)].map((m) => m[1]);
    const m = /export type RunnerState =([^;]+);/.exec(stripComments(ts));
    if (!m) throw new Error("type RunnerState not found");
    expect(new Set([...m[1].matchAll(/"([a-z]+)"/g)].map((x) => x[1]))).toEqual(new Set(go));
  });

  it.each([
    ["RunnerOwnerKind", /RunnerOwner(?:Person|Org)\s+RunnerOwnerKind = "([a-z_]+)"/g],
    ["RunnerTrustClass", /RunnerTrust(?:Personal|ManagedDevice|OrgCluster)\s+RunnerTrustClass = "([a-z_]+)"/g],
  ])("%s is the Go constants", (name, re) => {
    const go = [...read("internal/types/runner.go").matchAll(re as RegExp)].map((m) => m[1]);
    const m = new RegExp(`export type ${name} =([^;]+);`).exec(stripComments(ts));
    if (!m) throw new Error(`type ${name} not found`);
    expect(go.length).toBeGreaterThanOrEqual(2);
    expect(new Set([...m[1].matchAll(/"([a-z_]+)"/g)].map((x) => x[1]))).toEqual(new Set(go));
  });

  it("RunnerFailureStage is the Go runnerwire.Stage list", () => {
    const go = [...read("internal/runnerwire/stage.go").matchAll(/Stage\w+\s+Stage = "([a-z]+)"/g)].map((m) => m[1]);
    const m = /export type RunnerFailureStage =([^;]+);/.exec(stripComments(ts));
    if (!m) throw new Error("type RunnerFailureStage not found");
    expect(go.length).toBeGreaterThanOrEqual(12);
    expect(new Set([...m[1].matchAll(/"([a-z]+)"/g)].map((x) => x[1]))).toEqual(new Set(go));
  });

  it("RunnerManagementReason is the Go reason block", () => {
    const block = /The runners management routes \(runner_inventory\.go\)\.\nconst \(([\s\S]*?)\n\)/.exec(read("internal/api/reasons_routes.go"));
    if (!block) throw new Error("runners management reason block not found");
    const go = [...block[1].matchAll(/= "([a-z_]+)"/g)].map((m) => m[1]);
    const m = /export type RunnerManagementReason =([^;]+);/.exec(stripComments(ts));
    if (!m) throw new Error("type RunnerManagementReason not found");
    expect(new Set([...m[1].matchAll(/"([a-z_]+)"/g)].map((x) => x[1]))).toEqual(new Set(go));
    expect(go.length).toBeGreaterThanOrEqual(6);
  });

  it("the state filter is the Go constants", () => {
    const go = [...read("internal/types/runner_inventory.go").matchAll(/RunnerFilter\w+\s+RunnerFilter = "([a-z]+)"/g)].map((m) => m[1]);
    const m = /export type RunnerFilter =([^;]+);/.exec(stripComments(ts));
    if (!m) throw new Error("type RunnerFilter not found");
    expect(new Set([...m[1].matchAll(/"([a-z]+)"/g)].map((x) => x[1]))).toEqual(new Set(go));
  });
});
