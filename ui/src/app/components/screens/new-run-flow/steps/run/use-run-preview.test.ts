/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Run step recognises the inbound feature refusal by its opening sentence, because the wire carries only the
// shared reason capability_feature. This reads the Go literal as text and fails the moment either side's sentence moves.
import { describe, expect, it } from "vitest";
import { existsSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { INBOUND_REFUSAL } from "./use-run-preview";

function repoRoot(): string {
  let dir = resolve(process.cwd());
  for (let i = 0; i < 8; i++) {
    if (existsSync(join(dir, "go.mod"))) return dir;
    dir = dirname(dir);
  }
  throw new Error("go.mod not found walking up from " + process.cwd());
}

describe("inbound refusal sentence", () => {
  it("is the opening of inboundDeclaredRefusal's message in Go", () => {
    const src = readFileSync(join(repoRoot(), "internal/api/run_request_governance.go"), "utf8");
    const fn = /func \(s \*Server\) inboundDeclaredRefusal[\s\S]*?\n}\n/.exec(src);
    expect(fn).not.toBeNull(); // stale-regex guard
    const literal = /runCapabilityRefusal\([^"]*"runs\.ui_apps",\s*"((?:[^"\\]|\\.)*)"\)/.exec(fn![0]);
    expect(literal).not.toBeNull();
    expect(literal![1].startsWith(INBOUND_REFUSAL)).toBe(true);
  });
});
