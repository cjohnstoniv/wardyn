/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { ADO } from "./ado-entra-copy";
import { ADO_PAT } from "./ado-pat-copy";
import { patCardView } from "./ado-pat-display";
import { scmAccessCause, scmAccessChip, scmAccessNeedsConnect, scmOwnTokenAction } from "./scm-access-display";
import type { SCMAccess } from "./types";

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

// The approved own-token chip packet: a person's own token reads in Settings'
// words after the row's "Azure DevOps ·" prefix, in three tones (green only for
// a usable token that is not refused, amber for expiring, red for refused or
// expired). Dates are year 2000: always past, so the fixture-date gate skips them.
describe("scm-access display — a person's own token", () => {
  const now = new Date(2000, 9, 24, 12, 0).getTime();
  const own: SCMAccess = { state: "live", source: "own", token_mode: "own_pat", org: "https://dev.azure.com/wardyn-live-test", expires_on: "2000-11-27" };
  const refusedAt = new Date(2000, 9, 2, 9, 30).toISOString();
  const chip = (a: SCMAccess) => scmAccessChip(a.state, a.source, a.cause, a, now);
  const action = (a: SCMAccess) => scmOwnTokenAction(a);

  it("live reads Connected in success, with no line and no button", () => {
    expect(chip(own)).toEqual({ label: "Azure DevOps · Connected", tone: "success" });
    expect(action(own)).toBeNull();
  });

  it("expiring reads the days left in warning, with the expiry line and Replace", () => {
    const a = { ...own, state: "expiring", expires_on: "2000-10-27" };
    expect(chip(a)).toEqual({ label: "Azure DevOps · Expires in 3 days", tone: "warning" });
    expect(action(a)).toEqual({ line: ADO_PAT.OWN_EXPIRING_LINE("wardyn-live-test", "27 October"), button: "replace" });
  });

  it("refused reads Refused in danger, with the refusal line and Replace, over live", () => {
    const a = { ...own, expires_on: "2000-10-27", refused_at: refusedAt };
    expect(chip(a)).toEqual({ label: "Azure DevOps · Refused", tone: "danger" });
    expect(action(a)).toEqual({ line: ADO_PAT.OWN_REFUSED_LINE("2 October", "27 October"), button: "replace" });
  });

  it("refused wins over expiring", () => {
    const a = { ...own, state: "expiring", expires_on: "2000-10-27", refused_at: refusedAt };
    expect(chip(a)).toEqual({ label: "Azure DevOps · Refused", tone: "danger" });
    expect(action(a)?.line).toBe(ADO_PAT.OWN_REFUSED_LINE("2 October", "27 October"));
  });

  it("expired reads Expired in danger, with its line and Add, and wins over refused", () => {
    const a = { ...own, state: "expired_signin", cause: "token_expired", expires_on: "2000-10-01", refused_at: refusedAt };
    expect(chip(a)).toEqual({ label: "Azure DevOps · Expired", tone: "danger" });
    expect(action(a)).toEqual({ line: ADO_PAT.OWN_EXPIRED_BODY, button: "add" });
  });

  it("not added keeps the existing Not connected chip and offers Add with no line", () => {
    const a: SCMAccess = { state: "not_configured", token_mode: "own_pat", org: own.org };
    expect(chip(a)).toEqual({ label: ADO.ACCESS_NOT_CONNECTED, tone: "warning" });
    expect(action(a)).toEqual({ line: "", button: "add" });
  });

  it("says what Settings' own card says for every state", () => {
    const states: SCMAccess[] = [
      own,
      { ...own, state: "expiring", expires_on: "2000-10-27" },
      { ...own, expires_on: "2000-10-27", refused_at: refusedAt },
      { ...own, state: "expiring", expires_on: "2000-10-27", refused_at: refusedAt },
      { ...own, state: "expired_signin", cause: "token_expired" },
      { ...own, state: "expired_signin", cause: "token_expired", refused_at: refusedAt },
    ];
    for (const a of states) {
      const settings = patCardView(a, "Azure DevOps", now)!;
      expect(chip(a)?.label).toBe(`Azure DevOps · ${settings.chip.label}`);
      expect(chip(a)?.tone).toBe(settings.chip.tone);
    }
  });

  it("an admin-provided or sign-in row is unchanged", () => {
    expect(scmAccessChip("live", undefined, undefined, { state: "live" }, now)).toEqual({ label: ADO.ACCESS_SHARED_LIVE, tone: "success" });
    expect(scmAccessChip("live", "org", undefined, { state: "live", source: "org" }, now)).toEqual({ label: ADO.ACCESS_LIVE_ORG, tone: "success" });
    expect(scmOwnTokenAction({ state: "live" })).toBeNull();
    expect(scmOwnTokenAction({ state: "shared_expired" })).toBeNull();
    expect(scmOwnTokenAction({ state: "not_configured", cause: "row_is_newer" })).toBeNull();
  });
});
