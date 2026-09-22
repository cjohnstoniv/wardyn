/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// waitingAdoConsent's two strings are hand-copied literals in
// reauth-waiting-copy.ts (not imported from ado-entra-copy.ts — see that
// file's own top comment for the bundle-budget reason: this module is on
// the EAGER graph and ado-entra-copy.ts is ~300 lines of §7+§10 canon).
// A test file pays no bundle cost, so it's the cheap way to still catch
// drift between the two copies that an import would have caught for free.
import { describe, expect, it } from "vitest";
import { waitingAdoConsent, waitingReauth } from "./reauth-waiting-copy";
import { ADO } from "./ado-entra-copy";

describe("waitingAdoConsent — the hand-copied literals stay byte-identical to canon", () => {
  it("the 'mine' string matches ADO.WAITING_ADO_MINE", () => {
    expect(waitingAdoConsent(1, true)).toBe(ADO.WAITING_ADO_MINE);
  });

  it("the 'owner' string matches ADO.WAITING_ADO_OWNER", () => {
    expect(waitingAdoConsent(1, false)).toBe(ADO.WAITING_ADO_OWNER);
  });

  it("never says AWS", () => {
    expect(waitingAdoConsent(1, true)).not.toMatch(/AWS/);
    expect(waitingAdoConsent(1, false)).not.toMatch(/AWS/);
  });

  it("appends the count suffix past one, same shape as waitingReauth", () => {
    expect(waitingAdoConsent(2, true)).toBe(`${ADO.WAITING_ADO_MINE} · 1 more waiting`);
    expect(waitingAdoConsent(2, true)).toBe(waitingReauth(2, true).replace(/AWS/, "Azure DevOps"));
  });
});
