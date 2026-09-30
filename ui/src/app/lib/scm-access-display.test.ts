/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { ADO } from "./ado-entra-copy";
import { ADO_PAT } from "./ado-pat-copy";
import { scmAccessCause, scmAccessChip, scmAccessNeedsConnect } from "./scm-access-display";

// scmaccess.go's two expired_signin causes each render their own chip and
// cause line, and both offer the connect door.
describe("scm-access display — expired_signin", () => {
  it("an ended sign-in reads Disconnected, with the ended cause", () => {
    expect(scmAccessChip("expired_signin", "org", "ended")).toEqual({ label: ADO.ACCESS_EXPIRED, tone: "warning" });
    expect(scmAccessCause("ended")).toBe(ADO.CAUSE_ENDED);
  });

  it("a sign-in the row outgrew reads Needs your consent, with the consent cause", () => {
    expect(scmAccessChip("expired_signin", "org", "consent_needed")).toEqual({ label: ADO.ACCESS_NEEDS_CONSENT, tone: "warning" });
    expect(scmAccessCause("consent_needed")).toBe(ADO.CAUSE_CONSENT_NEEDED);
  });

  it("offers the connect door for not_configured and expired_signin only", () => {
    expect(scmAccessNeedsConnect("expired_signin")).toBe(true);
    expect(scmAccessNeedsConnect("not_configured")).toBe(true);
    expect(scmAccessNeedsConnect("live")).toBe(false);
  });
});

// #1428, #1430: the per-person token rows' causes (scmaccess.go), in the
// approved mock's words. Three are not fixed by signing in.
describe("scm-access display — per-person token causes", () => {
  it("each cause reads its own line", () => {
    expect(scmAccessCause("permissions_missing")).toBe(ADO_PAT.SIGN_IN_AGAIN_BODY);
    expect(scmAccessCause("blocked")).toBe(ADO_PAT.BLOCKED_BODY);
    expect(scmAccessCause("ado_pat_needs_console_app")).toBe(ADO_PAT.MEMBER_NEEDS_ADMIN);
    expect(scmAccessCause("ado_pat_needs_console_app")).not.toContain("WARDYN_OIDC_CLIENT_SECRET");
    expect(scmAccessCause("token_expired")).toBe(ADO_PAT.OWN_EXPIRED_BODY);
  });

  it("a blocked organisation, a missing client secret and an expired own token offer no connect door", () => {
    for (const cause of ["blocked", "ado_pat_needs_console_app", "token_expired"]) {
      expect(scmAccessNeedsConnect("expired_signin", cause)).toBe(false);
    }
    expect(scmAccessNeedsConnect("expired_signin", "permissions_missing")).toBe(true);
    expect(scmAccessNeedsConnect("expired_signin", "ended")).toBe(true);
    expect(scmAccessNeedsConnect("not_configured", undefined)).toBe(true);
  });
});
