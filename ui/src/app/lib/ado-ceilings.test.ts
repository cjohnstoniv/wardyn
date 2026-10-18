/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { orgCeilings, refusedBy, unionCeiling } from "./ado-ceilings";
import type { SCMAccess } from "./types";

const row = (org: string, capability_ceiling?: string[]): SCMAccess => ({ state: "live", org: `https://dev.azure.com/${org}`, capability_ceiling });

describe("the Azure DevOps ceiling across organisations (#2146)", () => {
  const cs = orgCeilings([row("acme", ["code_read", "pr"]), row("beta", ["code_read"])]);

  it("names each row's ceiling by organisation, in server order, skipping a row without one", () => {
    expect(orgCeilings([row("acme", ["code_read"]), row("none"), row("beta", [])])).toEqual([
      { org: "acme", ceiling: ["code_read"] },
      { org: "beta", ceiling: [] },
    ]);
  });

  it("unions the ceilings: a capability any organisation allows is not locked", () => {
    expect(unionCeiling(cs)).toEqual(["code_read", "pr"]);
  });

  it("is unknown (locks nothing) when no row names a ceiling", () => {
    expect(unionCeiling([])).toBeUndefined();
  });

  it("names the organisations that refuse a capability only some allow", () => {
    expect(refusedBy(cs, "pr")).toEqual(["beta"]);
  });

  it("hints nothing for a capability all allow, none allow (that is the lock), or a single organisation", () => {
    expect(refusedBy(cs, "code_read")).toEqual([]);
    expect(refusedBy(cs, "policy_admin")).toEqual([]);
    expect(refusedBy(orgCeilings([row("acme", ["code_read"])]), "pr")).toEqual([]);
  });
});
