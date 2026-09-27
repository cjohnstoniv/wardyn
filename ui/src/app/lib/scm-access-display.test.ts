/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { ADO } from "./ado-entra-copy";
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
