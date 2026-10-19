// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";
import { PERM, PERM_EXTRA } from "./permissions-copy";

// PERM.HINT_ALL is quoted verbatim in three design docs' "Reused canon" tables,
// which the governance/user-drives parity suites deliberately skip — so a
// correction at the source (W6 U-01) reached none of the copies. This holds
// the four spellings together: the constant is the canon, the docs quote it.
describe("permissions-copy: reused-canon rows quote the constant", () => {
  it.each([
    "../../../../docs/design/permissioning-prompt.md",
    "../../../../docs/design/governance-prompt.md",
    "../../../../docs/design/user-drives-prompt.md",
  ])("%s carries PERM.HINT_ALL byte-for-byte", (rel) => {
    const doc = readFileSync(new URL(rel, import.meta.url), "utf8");
    expect(doc).toContain(PERM.HINT_ALL);
  });
});

// service_attach is the one capability that binds super admins, the admin token
// and local mode (capAllowedAsMember); the "never bounded" lines must say so.
describe("permissions-copy: the exemption lines name the service_attach exception", () => {
  it("EXEMPT and HINT_ALL both mention service_attach", () => {
    expect(PERM.EXEMPT).toContain("service_attach");
    expect(PERM.HINT_ALL).toContain("service_attach");
  });
});

// A re-save of an inert row inserts a canonical twin and leaves the inert row
// in place, so the remedy must end with deleting it (permissions.go markInertGrants).
describe("permissions-copy: the inert-row remedy", () => {
  it("tells the admin to delete the old row", () => {
    expect(PERM_EXTRA.INERT_REMEDY).toMatch(/then delete this row/);
  });
});
