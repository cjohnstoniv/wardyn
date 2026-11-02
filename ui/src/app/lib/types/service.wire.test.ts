/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The service wire parity: each TS interface of types/runs.ts for a run's service has exactly the json tags
// of the Go struct in internal/types/service.go, the health states are exactly the Go constants, and
// AgentRun carries both fields. The Go side is read from source.

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

function goJSONTags(src: string, structName: string): string[] {
  const m = new RegExp(`type\\s+${structName}\\s+struct\\s*\\{([\\s\\S]*?)\\n\\}`).exec(src);
  if (!m) throw new Error(`struct ${structName} not found`);
  return [...m[1].matchAll(/json:"([^",]+)(?:,[^"]*)?"/g)].map((t) => t[1]).filter((t) => t !== "-");
}

function tsBody(src: string, iface: string): string {
  const m = new RegExp(`export\\s+interface\\s+${iface}\\b[^{]*\\{([\\s\\S]*?)\\n\\}`).exec(stripComments(src));
  if (!m) throw new Error(`interface ${iface} not found`);
  return m[1];
}

const tsKeys = (src: string, iface: string) =>
  [...tsBody(src, iface).matchAll(/^\s*([a-z0-9_]+)\??\s*:/gm)].map((k) => k[1]);

describe("run service wire", () => {
  const go = read("internal/types/service.go");
  const runsTs = read("ui/src/app/lib/types/runs.ts");

  it.each(["RunService", "ServiceHealth", "RunServiceStatus"])("%s has the Go struct's json tags", (name) => {
    expect(new Set(tsKeys(runsTs, name))).toEqual(new Set(goJSONTags(go, name)));
  });

  it("ServiceHealthState is the Go set", () => {
    const goStates = [...go.matchAll(/ServiceHealth\w+\s+ServiceHealthState = "([a-z]+)"/g)].map((m) => m[1]);
    const m = /export type ServiceHealthState =([^;]+);/.exec(runsTs);
    expect([...(m?.[1] ?? "").matchAll(/"([a-z]+)"/g)].map((x) => x[1])).toEqual(goStates);
    expect(goStates).toHaveLength(4);
  });

  it("AgentRun carries service and service_status as optional pointers", () => {
    const types = read("internal/types/types.go");
    expect(types).toMatch(/Service\s+\*RunService\s+`json:"service,omitempty"`/);
    expect(types).toMatch(/ServiceStatus\s+\*RunServiceStatus\s+`json:"service_status,omitempty"`/);
    const body = tsBody(runsTs, "AgentRun");
    expect(body).toMatch(/service\?: RunService;/);
    expect(body).toMatch(/service_status\?: RunServiceStatus;/);
  });
});
