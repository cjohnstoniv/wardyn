/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, expect, it } from "vitest";
import { SCIM } from "./scim-copy";

// The ledger's step keys are Go constants; a key the copy does not name renders
// as a raw identifier in the admin's "Unfinished deprovisioning" table.
const goSteps = ["scim_suspend.go", "scim_purge.go"].flatMap((n) => {
  const src = readFileSync(resolve(process.cwd(), "../internal/api", n), "utf8");
  return [...src.matchAll(/^\s*jobStep\w+\s*=\s*"([a-z_]+)"/gm)].map((m) => m[1]);
});

describe("scim-copy — STEP names every job step the Go ledger writes", () => {
  it("reads the step constants out of the Go source", () => {
    expect(goSteps).toEqual(expect.arrayContaining(["sweep", "erase", "audit_deprovision", "service_identities"]));
  });

  it.each([...new Set(goSteps)])("%s has a label", (step) => {
    expect(SCIM.STEP[step]).toBeTruthy();
  });
});
