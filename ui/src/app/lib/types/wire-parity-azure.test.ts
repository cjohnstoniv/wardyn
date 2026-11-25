/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Go->TS parity for ModelProvider.azure: the keys of Go's types.AzureSettings and of the console's AzureSettings,
// each read as TEXT from its source (wire-parity.test.ts does the same for the larger DTOs).

import { it, expect } from "vitest";
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

it("AzureSettings keeps the Go json tags and the TypeScript keys in parity", () => {
  const root = repoRoot();
  const go = readFileSync(join(root, "internal/types/model_provider.go"), "utf8");
  const ts = readFileSync(join(root, "ui/src/app/lib/types/site.ts"), "utf8");
  const goBody = /type AzureSettings struct \{([\s\S]*?)\n\}/.exec(go);
  const tsBody = /export interface AzureSettings \{([\s\S]*?)\n\}/.exec(ts);
  expect(goBody).not.toBeNull();
  expect(tsBody).not.toBeNull();
  const goTags = [...goBody![1].matchAll(/json:"([^",]+)/g)].map((m) => m[1]).sort();
  const tsKeys = [...tsBody![1].matchAll(/^\s*([a-z_]+)\??:/gm)].map((m) => m[1]).sort();
  expect(goTags).toEqual(["endpoint", "route"]);
  expect(tsKeys).toEqual(goTags);
});
