/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Go/TS parity for the secrets API reason the console reacts to.

import { existsSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { describe, expect, it } from "vitest";
import { SECRET_EXISTS_REASON } from "./secret-refusals";

function repoRoot(): string {
  let dir = resolve(process.cwd());
  for (let i = 0; i < 8; i++) {
    if (existsSync(join(dir, "go.mod"))) return dir;
    dir = dirname(dir);
  }
  throw new Error("go.mod not found");
}

describe("secret refusal reasons", () => {
  it("the create-only refusal is the server's reasonSecretExists", () => {
    const go = readFileSync(join(repoRoot(), "internal/api/reasons.go"), "utf8");
    expect(go).toMatch(new RegExp(`reasonSecretExists\\s+= "${SECRET_EXISTS_REASON}"`));
  });
});
