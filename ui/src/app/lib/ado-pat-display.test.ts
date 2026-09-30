/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import type { GitProvider } from "./api/providers";
import type { ADOOrgCheck, ADORunToken, SCMAccessPAT } from "./types/ado-pat";
import {
  adoIsServer,
  adoOrgLabel,
  adoRowNeedsChoice,
  adoTokenMode,
  adoTokensURL,
  daysLeft,
  formatClock,
  formatDay,
  newRunTokenCaps,
  orgCheckView,
  patCardView,
  patLifetimeInvalid,
  patRefusalNote,
  runTokenView,
} from "./ado-pat-display";
import { ADO_PAT_REASON, adoPatRefusalReason } from "./api/ado-pat";
import { HttpError } from "./api/core";

// Local-time instants, so the clock text is the same in any timezone.
const at = (h: number, m: number, day = 29) => new Date(2000, 8, day, h, m).toISOString();
const access = (over: Partial<SCMAccessPAT>): SCMAccessPAT => ({ state: "live", org: "https://dev.azure.com/wardyn-live-test", ...over }) as SCMAccessPAT;

describe("times", () => {
  it("formats a clock as 24-hour HH:MM and a day as 'D Month'", () => {
    expect(formatClock(at(9, 2))).toBe("09:02");
    expect(formatClock(at(0, 5))).toBe("00:05");
    expect(formatClock(at(17, 2))).toBe("17:02");
    expect(formatDay(new Date(2000, 9, 27).toISOString())).toBe("27 October");
  });
  it("reads a date-only string as a calendar day in the reader's zone, not midnight UTC", () => {
    expect(formatDay("2000-10-27")).toBe("27 October");
    expect(formatDay("2000-01-01")).toBe("1 January");
    const now = new Date(2000, 9, 24, 12, 0).getTime();
    expect(daysLeft("2000-10-27", now)).toBe(3);
  });
  it("counts whole days to an expiry, rounding up, never below zero", () => {
    const now = new Date(2000, 8, 29, 12, 0).getTime();
    expect(daysLeft(new Date(2000, 8, 30, 8, 0).toISOString(), now)).toBe(1);
    expect(daysLeft(new Date(2000, 9, 2, 12, 0).toISOString(), now)).toBe(3);
    expect(daysLeft(new Date(2000, 8, 1).toISOString(), now)).toBe(0);
  });
});

describe("organisation names and the token page", () => {
  it("names a dev.azure.com organisation by its first path segment", () => {
    expect(adoOrgLabel("https://dev.azure.com/wardyn-live-test")).toBe("wardyn-live-test");
    expect(adoOrgLabel("https://tfs.example.com/collection/")).toBe("tfs.example.com/collection");
  });
  it("links Azure DevOps' own token page", () => {
    expect(adoTokensURL("https://dev.azure.com/wardyn-live-test/some-project")).toBe(
      "https://dev.azure.com/wardyn-live-test/_usersSettings/tokens",
    );
    expect(adoTokensURL("https://tfs.example.com/collection")).toBe("https://tfs.example.com/collection/_usersSettings/tokens");
    expect(adoTokensURL("")).toBe("");
  });
});

describe("patCardView: a row that creates a token per run (states 4, 8b, 9)", () => {
  const minted = { token_mode: "minted_pat" } as const;
  it("not connected offers Connect", () => {
    const v = patCardView(access({ ...minted, state: "not_configured" }), "Azure DevOps")!;
    expect(v.chip).toEqual({ label: "Not connected", tone: "neutral" });
    expect(v.body).toEqual([
      "Connect once so Wardyn can create a short-lived token for each of your runs. Each token has only that run's access and is revoked when the run ends.",
    ]);
    expect(v.action).toBe("connect");
    expect(v.disconnect).toBe(false);
  });
  it("connected says how tokens are made, names the last finished token, offers Disconnect", () => {
    const v = patCardView(access({ ...minted, last_token: { created_at: at(9, 2), revoked_at: at(9, 41) } }), "Azure DevOps")!;
    expect(v.chip).toEqual({ label: "Connected", tone: "success" });
    expect(v.body).toEqual([
      "Tokens are created in your name, one per run, and revoked when it ends. Azure DevOps lists them under Personal access tokens as 'Wardyn run …'.",
      "Last token: created 09:02, revoked 09:41.",
    ]);
    expect(v.action).toBeNull();
    expect(v.disconnect).toBe(true);
  });
  it("connected with a token still live names no last token", () => {
    const v = patCardView(access({ ...minted, last_token: { created_at: at(9, 2) } }), "Azure DevOps")!;
    expect(v.body).toHaveLength(1);
  });
  it("sign in again, whichever cause ended the connection, offers Connect", () => {
    for (const cause of ["ended", "consent_needed", "permissions_missing", undefined]) {
      const v = patCardView(access({ ...minted, state: "expired_signin", cause, source: "org" }), "Azure DevOps")!;
      expect(v.chip).toEqual({ label: "Sign in again", tone: "warning" });
      expect(v.body).toEqual(["Your organisation asked you to sign in again before Wardyn can create tokens."]);
      expect(v.action).toBe("connect");
    }
  });
  it("blocked by the organisation (the blocked cause) names the fix and offers nothing to press", () => {
    const v = patCardView(access({ ...minted, state: "expired_signin", cause: "blocked" }), "Azure DevOps")!;
    expect(v.chip).toEqual({ label: "Blocked by your organisation", tone: "danger" });
    expect(v.body).toEqual([
      "Your organisation doesn't let you create personal access tokens. Ask an Azure DevOps administrator to add you to the allow list.",
    ]);
    expect(v.action).toBeNull();
  });
  it("a row the console cannot redeem is the admin's to fix: the chip and a member sentence, never the admin's", () => {
    const v = patCardView(access({ ...minted, state: "expired_signin", cause: "ado_pat_needs_console_app" }), "Azure DevOps")!;
    expect(v.chip).toEqual({ label: "Not connected", tone: "neutral" });
    expect(v.body).toEqual(["Your administrator needs to finish setting up Azure DevOps before runs can use it."]);
    expect(v.action).toBeNull();
    expect(v.disconnect).toBe(false);
  });
  it("an unrecognised state draws nothing", () => {
    expect(patCardView(access({ ...minted, state: "mystery" }), "Azure DevOps")).toBeNull();
  });
});

describe("patCardView: a row where each person adds their own token (state 10)", () => {
  const own = { token_mode: "own_pat" } as const;
  const now = new Date(2000, 9, 24, 12, 0).getTime();
  it("no token yet offers Add", () => {
    const v = patCardView(access({ ...own, state: "not_configured" }), "Azure DevOps", now)!;
    expect(v.chip).toEqual({ label: "Not connected", tone: "neutral" });
    expect(v.action).toBe("add_token");
  });
  it("expiring counts the days and offers Replace", () => {
    const v = patCardView(access({ ...own, state: "expiring", expires_on: "2000-10-27" }), "Azure DevOps", now)!;
    expect(v.chip).toEqual({ label: "Expires in 3 days", tone: "warning" });
    expect(v.body).toEqual(["Your token for wardyn-live-test expires on 27 October."]);
    expect(v.action).toBe("replace_token");
  });
  it("live shows the expiry line under Connected", () => {
    const v = patCardView(access({ ...own, expires_on: "2000-10-27" }), "Azure DevOps", now)!;
    expect(v.chip).toEqual({ label: "Connected", tone: "success" });
    expect(v.body).toEqual(["Your token for wardyn-live-test expires on 27 October."]);
  });
  it("expired (expired_signin, the token_expired cause) says runs cannot reach Azure DevOps and offers Add", () => {
    const v = patCardView(access({ ...own, state: "expired_signin", cause: "token_expired" }), "Azure DevOps", now)!;
    expect(v.chip).toEqual({ label: "Expired", tone: "danger" });
    expect(v.body).toEqual(["Your runs can't reach Azure DevOps until you add a new token."]);
    expect(v.action).toBe("add_token");
  });
  it("refused before expiry (#1445) reads Refused, names both days in one line and makes Replace primary", () => {
    const v = patCardView(access({ ...own, expires_on: "2000-10-27", refused_at: new Date(2000, 9, 2, 9, 30).toISOString() }), "Azure DevOps", now)!;
    expect(v.chip).toEqual({ label: "Refused", tone: "danger" });
    expect(v.body).toEqual(["Azure DevOps refused this token on 2 October, before it expires on 27 October. Replace it."]);
    expect(v.action).toBe("replace_token");
    expect(v.refused).toBe(true);
  });
  it("an expired row ignores refused_at: the token is gone either way, so expired wins", () => {
    const v = patCardView(access({ ...own, state: "expired_signin", cause: "token_expired", expires_on: "2000-10-01", refused_at: new Date(2000, 8, 20, 9, 30).toISOString() }), "Azure DevOps", now)!;
    expect(v.chip).toEqual({ label: "Expired", tone: "danger" });
    expect(v.body).toEqual(["Your runs can't reach Azure DevOps until you add a new token."]);
    expect(v.action).toBe("add_token");
    expect(v.refused).toBeUndefined();
  });
  it("a live row without refused_at is not refused", () => {
    expect(patCardView(access({ ...own, expires_on: "2000-10-27" }), "Azure DevOps", now)!.refused).toBeUndefined();
  });
  it("an Azure DevOps Server row is titled as one and says it is git only", () => {
    const v = patCardView(access({ ...own, org: "https://tfs.example.com/collection" }), "Azure DevOps", now)!;
    expect(v.title).toBe("Azure DevOps Server");
    expect(v.chip).toEqual({ label: "Connected", tone: "success" });
    expect(v.body).toContain("Git only. Azure DevOps Server has no Entra sign-in.");
    expect(v.action).toBe("replace_token");
  });
});

it("a row on the Entra sign-in lane has no token card", () => {
  expect(patCardView(access({ token_mode: "bearer" }), "Azure DevOps")).toBeNull();
  expect(patCardView(access({}), "Azure DevOps")).toBeNull();
});

describe("adoIsServer", () => {
  it("is any host that is not dev.azure.com or *.visualstudio.com", () => {
    expect(adoIsServer("https://dev.azure.com/o")).toBe(false);
    expect(adoIsServer("https://contoso.visualstudio.com/x")).toBe(false);
    expect(adoIsServer("https://tfs.example.com/collection")).toBe(true);
    expect(adoIsServer("not a url")).toBe(false);
  });
});

describe("orgCheckView: the check's answer as the mock's lines and alerts", () => {
  const base: ADOOrgCheck = { checked_at: at(9, 12), organisation: "o", pat_max_hours: 8, permissions: "granted" };
  it("permissions granted and the lifespan limit on with the row's life accepted: both good lines", () => {
    expect(orgCheckView({ ...base, token_life: "accepted", lifespan: "on" })).toEqual({
      permissions: "granted",
      lifespan: { state: "on", hours: 8 },
      tooLong: false,
      blocked: false,
    });
  });
  it("permissions missing stops at that line", () => {
    expect(orgCheckView({ ...base, permissions: "missing" })).toEqual({ permissions: "missing", lifespan: null, tooLong: false, blocked: false });
  });
  it("the lifespan limit off is its own line", () => {
    expect(orgCheckView({ ...base, token_life: "accepted", lifespan: "off" }).lifespan).toEqual({ state: "off" });
  });
  it("an unknown lifespan names what Azure DevOps answered (round 2), and draws nothing when it says none", () => {
    expect(orgCheckView({ ...base, token_life: "accepted", lifespan: "unknown", lifespan_error: "invalidValidTo" }).lifespan).toEqual({
      state: "unknown",
      error: "invalidValidTo",
    });
    expect(orgCheckView({ ...base, token_life: "accepted", lifespan: "unknown" }).lifespan).toBeNull();
  });
  it("the row's life refused for the lifespan policy is the too-long alert, not a good line", () => {
    const v = orgCheckView({ ...base, token_life: "refused", refusal: "ado_pat_lifespan_policy", lifespan: "on" });
    expect(v.tooLong).toBe(true);
    expect(v.lifespan).toBeNull();
  });
  it("a create refused on the organisation's policy is the blocked banner", () => {
    const v = orgCheckView({ ...base, token_life: "refused", refusal: "ado_pat_policy_blocked", lifespan: "unknown" });
    expect(v.blocked).toBe(true);
    expect(v.tooLong).toBe(false);
  });
  it("a token life refused for the lifespan policy is too long, not blocked", () => {
    const v = orgCheckView({ ...base, token_life: "refused", refusal: "ado_pat_lifespan_policy", lifespan: "on" });
    expect(v.blocked).toBe(false);
  });
});

describe("newRunTokenCaps", () => {
  it("prefers the policy's capabilities, then the row's default, then nothing", () => {
    expect(newRunTokenCaps(["code_read"], ["pr"])).toHaveLength(1);
    expect(newRunTokenCaps([], ["code_read", "pr"])).toHaveLength(2);
    expect(newRunTokenCaps(undefined, undefined)).toEqual([]);
  });
});

describe("runTokenView: the run page's token list (state 6, 7)", () => {
  const READ = ["vso.code", "vso.project"];
  const tok = (over: Partial<ADORunToken>): ADORunToken => ({ created_at: at(9, 2), valid_to: at(17, 2), scope: READ, ...over });
  it("6a: an active token is one plain line", () => {
    const v = runTokenView([tok({})], false);
    expect(v.lines).toEqual([{ text: "Azure DevOps token: created 09:02 · expires 17:02", old: false }]);
    expect(v.paused).toBe(false);
  });
  it("6b: a renewed token is never revoked early: the older one reads (renewed), from its position", () => {
    const v = runTokenView([tok({ created_at: at(15, 2), valid_to: at(23, 2) }), tok({ revoke_reason: "expired", revoked_at: at(17, 2) })], false);
    expect(v.lines).toEqual([
      { text: "Azure DevOps token: created 09:02 · expires 17:02 (renewed)", old: true },
      { text: "Azure DevOps token: created 15:02 · expires 23:02", old: false },
    ]);
    // Position alone decides it: the same with no reason at all, and no "revoked" word.
    const bare = runTokenView([tok({ created_at: at(15, 2), valid_to: at(23, 2) }), tok({})], false);
    expect(bare.lines[0].text).toBe("Azure DevOps token: created 09:02 · expires 17:02 (renewed)");
    expect(bare.lines[0].text).not.toMatch(/revoked/);
  });
  it("an expired token that nothing replaced reads (expired), never revoked", () => {
    const v = runTokenView([tok({ revoke_reason: "expired", revoked_at: at(17, 2) })], false);
    expect(v.lines).toEqual([{ text: "Azure DevOps token: created 09:02 · expires 17:02 (expired)", old: true }]);
  });
  it("6c: a paused run says its token was revoked", () => {
    const v = runTokenView([tok({ revoked_at: at(11, 30), revoke_reason: "pause" })], true);
    expect(v.lines[0]).toEqual({ text: "Azure DevOps token: created 09:02 · expires 17:02 · revoked 11:30 (paused)", old: true });
    expect(v.paused).toBe(true);
  });
  it("6d: the last token of a finished run stays plain, each reason the server writes reads as its words", () => {
    const reasons: Record<string, string> = {
      run_end: "run ended",
      kill: "run stopped",
      pause: "paused",
      drift: "access changed",
      disconnect: "disconnected",
      sweep: "cleaned up after a restart",
      upstream_401: "rejected by Azure DevOps",
      offboarding: "person removed",
    };
    for (const [wire, words] of Object.entries(reasons)) {
      const v = runTokenView([tok({ revoked_at: at(9, 41), revoke_reason: wire })], false);
      expect(v.lines[0]).toEqual({ text: `Azure DevOps token: created 09:02 · expires 17:02 · revoked 09:41 (${words})`, old: false });
    }
  });
  it("a reason this console has no words for adds no uncopied text", () => {
    const v = runTokenView([tok({ revoked_at: at(9, 41), revoke_reason: "mystery" })], false);
    expect(v.lines[0].text).toBe("Azure DevOps token: created 09:02 · expires 17:02");
  });
  it("6f: a revoke that was abandoned names when the token expires on its own", () => {
    expect(runTokenView([tok({ revoke_failed: true })], false).revokeFailedAt).toEqual(["17:02"]);
  });
  it("7: a token whose successor's scope is a strict superset reads (access added), whenever it was replaced", () => {
    // Replaced at 09:20, 18 minutes into an eight-hour life: the timing says nothing, the scope does.
    const v = runTokenView([tok({ created_at: at(9, 20), valid_to: at(17, 20), scope: [...READ, "vso.code_write"] }), tok({})], false);
    expect(v.lines.map((l) => l.text)).toEqual([
      "Azure DevOps token: created 09:02 · expires 17:02 (access added)",
      "Azure DevOps token: created 09:20 · expires 17:20",
    ]);
  });
  it("an equal, narrower or unrelated successor scope is a renewal, whenever it came", () => {
    for (const next of [READ, ["vso.code"], ["vso.code", "vso.work"], ["vso.build"]]) {
      const v = runTokenView([tok({ created_at: at(9, 20), valid_to: at(17, 20), scope: next }), tok({})], false);
      expect(v.lines[0].text).toBe("Azure DevOps token: created 09:02 · expires 17:02 (renewed)");
    }
  });
  it("scope order and duplicates do not matter to a widening", () => {
    const v = runTokenView([tok({ created_at: at(9, 20), scope: ["vso.project", "vso.code_write", "vso.code"] }), tok({})], false);
    expect(v.lines[0].text).toBe("Azure DevOps token: created 09:02 · expires 17:02 (access added)");
  });
  it("lists oldest first whatever order the wire sends", () => {
    const v = runTokenView([tok({ created_at: at(15, 2), valid_to: at(23, 2) }), tok({})], false);
    expect(v.lines[0].text).toContain("created 09:02");
  });
});

describe("the admin's row", () => {
  const row = (over: Partial<GitProvider>): GitProvider => ({ id: "azure_devops", kind: "azure_devops", base_urls: ["https://dev.azure.com/o"], ...over });
  it("reads no token_mode as the Entra sign-in", () => {
    expect(adoTokenMode(row({}))).toBe("bearer");
    expect(adoTokenMode(row({ entra: { tenant_id: "", client_id: "", token_mode: "own_pat" } }))).toBe("own_pat");
  });
  it("a switched-off own-token Services row waits for a choice", () => {
    expect(
      adoRowNeedsChoice(row({ disabled: true, lanes: ["entra"], credential_source: "per_user", entra: { tenant_id: "", client_id: "", token_mode: "own_pat" } })),
    ).toBe(true);
  });
  it("a switched-off git-only Server row waits for a choice", () => {
    expect(adoRowNeedsChoice(row({ disabled: true, lanes: ["pat"], credential_source: "per_user" }))).toBe(true);
  });
  it("a row that is on, shared, or on the sign-in lane does not", () => {
    const own = { tenant_id: "", client_id: "", token_mode: "own_pat" as const };
    expect(adoRowNeedsChoice(row({ lanes: ["entra"], credential_source: "per_user", entra: own }))).toBe(false);
    expect(adoRowNeedsChoice(row({ disabled: true, lanes: ["pat"], credential_source: "shared" }))).toBe(false);
    expect(adoRowNeedsChoice(row({ disabled: true, lanes: ["entra"], credential_source: "per_user", entra: { tenant_id: "t", client_id: "c" } }))).toBe(false);
    expect(adoRowNeedsChoice({ ...row({ disabled: true }), kind: "github" })).toBe(false);
  });
  it("refuses only a lifetime the server would refuse, in the mode that reads it", () => {
    const base = { tenant_id: "t", client_id: "c" };
    expect(patLifetimeInvalid(undefined)).toBe(false);
    expect(patLifetimeInvalid({ ...base, token_mode: "minted_pat" })).toBe(false);
    expect(patLifetimeInvalid({ ...base, token_mode: "minted_pat", pat_max_hours: 168 })).toBe(false);
    expect(patLifetimeInvalid({ ...base, token_mode: "minted_pat", pat_max_hours: 400 })).toBe(true);
    expect(patLifetimeInvalid({ ...base, token_mode: "minted_pat", pat_max_hours: 0 })).toBe(true);
    expect(patLifetimeInvalid({ ...base, token_mode: "own_pat", pat_max_days: 91 })).toBe(true);
    expect(patLifetimeInvalid({ ...base, token_mode: "own_pat", pat_max_days: 90 })).toBe(false);
    // Hours are ignored on an own-token row, and days on a minted one.
    expect(patLifetimeInvalid({ ...base, token_mode: "own_pat", pat_max_hours: 400 })).toBe(false);
    expect(patLifetimeInvalid({ ...base, token_mode: "minted_pat", pat_max_days: 400 })).toBe(false);
  });
});

describe("the wire refusal reasons (internal/api/reasons.go, docs/sdk.md)", () => {
  it("are exactly the four strings the server sends", () => {
    expect(ADO_PAT_REASON).toEqual({
      POLICY_BLOCKED: "ado_pat_policy_blocked",
      LIFESPAN_POLICY: "ado_pat_lifespan_policy",
      CONSENT_NEEDED: "ado_pat_consent_needed",
      MINT_REFUSED: "ado_pat_mint_refused",
    });
  });
  it("read off an error's reason, and nothing off any other", () => {
    expect(adoPatRefusalReason(new HttpError(403, "x", "ado_pat_policy_blocked"))).toBe("ado_pat_policy_blocked");
    expect(adoPatRefusalReason(new HttpError(422, "x", "git_credential"))).toBe("");
    expect(adoPatRefusalReason(new Error("ado_pat_policy_blocked"))).toBe("");
  });
  it("pick the launch note: blocked asks for the allow list, consent asks to connect again, the rest the strip alone", () => {
    expect(patRefusalNote("ado_pat_policy_blocked")).toBe("blocked");
    expect(patRefusalNote("ado_pat_consent_needed")).toBe("connect");
    expect(patRefusalNote("ado_pat_lifespan_policy")).toBeNull();
    expect(patRefusalNote("ado_pat_mint_refused")).toBeNull();
    expect(patRefusalNote("")).toBeNull();
  });
});
