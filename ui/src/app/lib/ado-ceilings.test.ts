/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { adoAllows, orgCeilings, refusedBy, unionCeiling } from "./ado-ceilings";
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

  it("takes a single row's own ceiling, so one organisation locks against its row and hints nothing", () => {
    const one = orgCeilings([row("acme", ["code_read"])]);
    expect(unionCeiling(one)).toEqual(["code_read"]);
    expect(refusedBy(one, "code_read")).toEqual([]);
  });
});

describe("pr on a ceiling stands for its three parts (adoscope.Has)", () => {
  it("allows each part under pr, and pr only under pr or all three parts", () => {
    for (const part of ["pr_create", "pr_vote", "pr_complete"]) expect(adoAllows(["code_read", "pr"], part), part).toBe(true);
    expect(adoAllows(["pr"], "pr")).toBe(true);
    expect(adoAllows(["pr_create", "pr_vote", "pr_complete"], "pr")).toBe(true);
    expect(adoAllows(["pr_create", "pr_vote"], "pr")).toBe(false);
    expect(adoAllows(["pr_create"], "pr_vote")).toBe(false);
    expect(adoAllows(["code_read"], "code_read")).toBe(true);
    expect(adoAllows(["code_read"], "code_write")).toBe(false);
  });

  it("does not name an organisation whose pr allows the part asked about", () => {
    const cs = orgCeilings([row("acme", ["pr"]), row("beta", ["pr_create"])]);
    expect(refusedBy(cs, "pr_create")).toEqual([]);
    expect(refusedBy(cs, "pr_vote")).toEqual(["beta"]);
  });
});
