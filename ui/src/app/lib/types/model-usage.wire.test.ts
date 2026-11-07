/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Go->TS wire parity for GET /runs/{id}/model-usage. The Go structs and the TS interfaces are read as text and
// diffed, so a field added on one side fails here rather than as an undefined at run time. The card's reason
// words and vendor names are checked against the Go sources that emit them.
import { describe, it, expect } from "vitest";
import { existsSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { MODEL_USAGE_REASON_CODES, MODEL_USAGE_VENDORS } from "../../components/wardyn/copy/model-usage";

function repoRoot(): string {
  let dir = resolve(process.cwd());
  for (let i = 0; i < 8; i++) {
    if (existsSync(join(dir, "go.mod"))) return dir;
    dir = dirname(dir);
  }
  throw new Error("go.mod not found walking up from " + process.cwd());
}

const root = repoRoot();
const viewGo = readFileSync(join(root, "internal/api/run_model_usage.go"), "utf8");
const parserGo = readFileSync(join(root, "internal/modelusage/usage.go"), "utf8");
const proxyGo = readFileSync(join(root, "internal/egress/proxy/model_usage.go"), "utf8");
const usageTs = readFileSync(join(root, "ui/src/app/lib/types/model-usage.ts"), "utf8");

// The json tags of one Go struct body; an embedded struct's own tags are added by the caller.
function goJSONTags(src: string, structName: string): string[] {
  const m = new RegExp(`type\\s+${structName}\\s+struct\\s*\\{([\\s\\S]*?)\\n\\}`).exec(src);
  if (!m) throw new Error(`struct ${structName} not found`);
  return [...m[1].matchAll(/json:"([^",]+)(?:,[^"]*)?"/g)].map((t) => t[1]);
}

function tsInterfaceKeys(src: string, name: string): string[] {
  const m = new RegExp(`export\\s+interface\\s+${name}\\b[^{]*\\{([\\s\\S]*?)\\n\\}`).exec(src.replace(/\/\*[\s\S]*?\*\//g, "").replace(/\/\/.*$/gm, ""));
  if (!m) throw new Error(`interface ${name} not found`);
  const keys: string[] = [];
  for (const line of m[1].split("\n")) {
    const k = /^\s*([A-Za-z_][A-Za-z0-9_]*)\??\s*:/.exec(line);
    if (k) keys.push(k[1]);
  }
  return keys;
}

describe("model usage wire parity", () => {
  it("ModelUsageTokens has every Go tag and nothing extra", () => {
    const goTags = goJSONTags(viewGo, "ModelUsageTokens");
    expect(goTags.length).toBe(8);
    expect(new Set(tsInterfaceKeys(usageTs, "ModelUsageTokens"))).toEqual(new Set(goTags));
  });

  it("ModelUsageModel is its own Go tags, and the TS interface extends the token counts", () => {
    const own = goJSONTags(viewGo, "ModelUsageModel");
    // The Go struct embeds ModelUsageTokens, which carries no tag of its own on that line.
    expect(own).toEqual(["provider", "vendor", "host", "model", "uncounted_reasons"]);
    expect(new Set(tsInterfaceKeys(usageTs, "ModelUsageModel"))).toEqual(new Set(own));
    expect(usageTs).toMatch(/interface ModelUsageModel extends ModelUsageTokens/);
  });

  it("ModelUsage (Go ModelUsageView) has every Go tag and nothing extra", () => {
    const goTags = goJSONTags(viewGo, "ModelUsageView");
    expect(goTags.length).toBe(4);
    expect(new Set(tsInterfaceKeys(usageTs, "ModelUsage"))).toEqual(new Set(goTags));
  });

  it("the card words every uncounted reason the parser can give", () => {
    const goReasons = [...parserGo.matchAll(/Reason\w+\s+Reason\s*=\s*"([a-z_]+)"/g)].map((m) => m[1]);
    expect(goReasons.length).toBe(7);
    // stream_cut is a partial reason, worded by the cut-off line and never an uncounted reason.
    expect(new Set(MODEL_USAGE_REASON_CODES)).toEqual(new Set(goReasons.filter((r) => r !== "stream_cut")));
  });

  it("the card names every vendor dialect the proxy records", () => {
    const fn = /func usageVendor\([\s\S]*?\n\}/.exec(proxyGo);
    if (!fn) throw new Error("usageVendor not found");
    const goVendors = [...fn[0].matchAll(/return "([a-z]+)"/g)].map((m) => m[1]);
    expect(goVendors.length).toBe(4);
    expect(new Set(MODEL_USAGE_VENDORS)).toEqual(new Set(goVendors));
  });
});
