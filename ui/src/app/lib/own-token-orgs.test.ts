/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { ADO_PAT } from "./ado-pat-copy";
import { joinOrgs } from "./ado-pat-display";
import { ownTokenOrgLine, ownTokenOrgsNeedingToken } from "./scm-access-display";
import type { SCMAccess } from "./types";

const own = (org: string, state: string, extra: Partial<SCMAccess> = {}): SCMAccess => ({
  state,
  token_mode: "own_pat",
  org: `https://dev.azure.com/${org}`,
  ...extra,
});

describe("several own-token organisations (#2146)", () => {
  const now = new Date(2000, 9, 7).getTime();

  it("joinOrgs reads as a sentence list", () => {
    expect(joinOrgs([])).toBe("");
    expect(joinOrgs(["acme"])).toBe("acme");
    expect(joinOrgs(["acme", "beta"])).toBe("acme and beta");
    expect(joinOrgs(["acme", "beta", "gamma"])).toBe("acme, beta and gamma");
  });

  it("a line per organisation: No token yet with Add, the expiry with nothing, expired with Add", () => {
    expect(ownTokenOrgLine(own("beta", "not_configured"), now)).toEqual({ org: "beta", label: ADO_PAT.OWN_NO_TOKEN, tone: "neutral", button: "add" });
    expect(ownTokenOrgLine(own("acme", "live", { expires_on: "2000-11-27" }), now)).toEqual({ org: "acme", label: ADO_PAT.CHIP_CONNECTED, tone: "success", button: null });
    expect(ownTokenOrgLine(own("acme", "expiring", { expires_on: "2000-10-27" }), now)).toMatchObject({ label: ADO_PAT.OWN_CHIP_EXPIRING(20), tone: "warning", button: "replace" });
    expect(ownTokenOrgLine(own("acme", "expired_signin", { cause: "token_expired" }), now)).toMatchObject({ label: ADO_PAT.OWN_CHIP_EXPIRED, tone: "danger", button: "add" });
  });

  it("a launch needs a token for the organisations with none stored or an expired one, never a live one", () => {
    const rows = [own("acme", "live"), own("beta", "not_configured"), own("gamma", "expired_signin", { cause: "token_expired" }), own("delta", "expiring")];
    expect(ownTokenOrgsNeedingToken(rows)).toEqual(["beta", "gamma"]);
  });

  it("the sentence names them: Add your Azure DevOps token for acme and beta first.", () => {
    expect(ADO_PAT.OWN_ORGS_ADD_FIRST(joinOrgs(["acme", "beta"]))).toBe("Add your Azure DevOps token for acme and beta first.");
  });
});
