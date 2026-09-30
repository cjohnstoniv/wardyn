/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import type { GitProvider } from "./api/providers";
import type { ADORunToken, SCMAccessPAT } from "./types/ado-pat";
import {
  adoOrgLabel,
  adoRowNeedsChoice,
  adoTokenMode,
  adoTokensURL,
  daysLeft,
  formatClock,
  formatDay,
  newRunTokenCaps,
  ownTokenScopeLabels,
  patCardView,
  patLifetimeInvalid,
  patRefusalNote,
  runTokenView,
} from "./ado-pat-display";
import { ADO_PAT_REASON, adoPatRefusalReason } from "./api/ado-pat";
import { HttpError } from "./api/core";

// Local-time instants, so the clock text is the same in any timezone.
const at = (h: number, m: number, day = 29) => new Date(2026, 8, day, h, m).toISOString();
const access = (over: Partial<SCMAccessPAT>): SCMAccessPAT => ({ state: "live", org: "https://dev.azure.com/wardyn-live-test", ...over }) as SCMAccessPAT;

describe("times", () => {
  it("formats a clock as 24-hour HH:MM and a day as 'D Month'", () => {
    expect(formatClock(at(9, 2))).toBe("09:02");
    expect(formatClock(at(0, 5))).toBe("00:05");
    expect(formatClock(at(17, 2))).toBe("17:02");
    expect(formatDay(new Date(2026, 9, 27).toISOString())).toBe("27 October");
  });
  it("counts whole days to an expiry, rounding up, never below zero", () => {
    const now = new Date(2026, 8, 29, 12, 0).getTime();
    expect(daysLeft(new Date(2026, 8, 30, 8, 0).toISOString(), now)).toBe(1);
    expect(daysLeft(new Date(2026, 9, 2, 12, 0).toISOString(), now)).toBe(3);
    expect(daysLeft(new Date(2026, 8, 1).toISOString(), now)).toBe(0);
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
    for (const cause of ["ended", "consent_needed", undefined]) {
      const v = patCardView(access({ ...minted, state: "expired_signin", cause }), "Azure DevOps")!;
      expect(v.chip).toEqual({ label: "Sign in again", tone: "warning" });
      expect(v.body).toEqual(["Your organisation asked you to sign in again before Wardyn can create tokens."]);
      expect(v.action).toBe("connect");
    }
  });
  it("blocked by the organisation names the fix and offers nothing to press", () => {
    const v = patCardView(access({ ...minted, state: "blocked" }), "Azure DevOps")!;
    expect(v.chip).toEqual({ label: "Blocked by your organisation", tone: "danger" });
    expect(v.body).toEqual([
      "Your organisation doesn't let you create personal access tokens. Ask an Azure DevOps administrator to add you to the allow list.",
    ]);
    expect(v.action).toBeNull();
  });
  it("an unrecognised state draws nothing", () => {
    expect(patCardView(access({ ...minted, state: "mystery" }), "Azure DevOps")).toBeNull();
  });
});

describe("patCardView: a row where each person adds their own token (state 10)", () => {
  const own = { token_mode: "own_pat" } as const;
  const now = new Date(2026, 9, 24, 12, 0).getTime();
  it("no token yet offers Add", () => {
    const v = patCardView(access({ ...own, state: "not_configured" }), "Azure DevOps", now)!;
    expect(v.chip).toEqual({ label: "Not connected", tone: "neutral" });
    expect(v.action).toBe("add_token");
  });
  it("expiring counts the days and offers Replace", () => {
    const v = patCardView(access({ ...own, state: "expiring", expires_at: new Date(2026, 9, 27, 12, 0).toISOString() }), "Azure DevOps", now)!;
    expect(v.chip).toEqual({ label: "Expires in 3 days", tone: "warning" });
    expect(v.body).toEqual(["Your token for wardyn-live-test expires on 27 October."]);
    expect(v.action).toBe("replace_token");
  });
  it("live shows the expiry line under Connected", () => {
    const v = patCardView(access({ ...own, expires_at: new Date(2026, 9, 27).toISOString() }), "Azure DevOps", now)!;
    expect(v.chip).toEqual({ label: "Connected", tone: "success" });
    expect(v.body).toEqual(["Your token for wardyn-live-test expires on 27 October."]);
  });
  it("expired says runs cannot reach Azure DevOps and offers Add", () => {
    const v = patCardView(access({ ...own, state: "expired" }), "Azure DevOps", now)!;
    expect(v.chip).toEqual({ label: "Expired", tone: "danger" });
    expect(v.body).toEqual(["Your runs can't reach Azure DevOps until you add a new token."]);
    expect(v.action).toBe("add_token");
  });
  it("an Azure DevOps Server row is titled as one and says it is git only", () => {
    const v = patCardView(access({ ...own, server: true }), "Azure DevOps", now)!;
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

describe("ownTokenScopeLabels: Azure DevOps' own wording, from the row's scopes (Q7)", () => {
  it("labels the scopes as the token page does, dropping a read a write covers", () => {
    expect(ownTokenScopeLabels(["vso.code", "vso.code_write", "vso.project", "vso.work"])).toEqual([
      "Code (Read & write)",
      "Project and Team (Read)",
      "Work Items (Read)",
    ]);
  });
  it("names an unknown scope as itself and an absent list as nothing", () => {
    expect(ownTokenScopeLabels(["vso.something_new"])).toEqual(["vso.something_new"]);
    expect(ownTokenScopeLabels(undefined)).toEqual([]);
  });
});

describe("newRunTokenCaps", () => {
  it("prefers the policy's capabilities, then the row's default, then nothing", () => {
    expect(newRunTokenCaps(["read"], ["pr"])).toHaveLength(1);
    expect(newRunTokenCaps([], ["read", "pr"])).toHaveLength(2);
    expect(newRunTokenCaps(undefined, undefined)).toEqual([]);
  });
});

describe("runTokenView: the run page's token list (state 6, 7)", () => {
  const tok = (over: Partial<ADORunToken>): ADORunToken => ({ created_at: at(9, 2), valid_to: at(17, 2), ...over });
  it("6a: an active token is one plain line", () => {
    const v = runTokenView([tok({})], false);
    expect(v.lines).toEqual([{ text: "Azure DevOps token: created 09:02 · expires 17:02", old: false }]);
    expect(v.added).toEqual([]);
    expect(v.paused).toBe(false);
  });
  it("6b: a renewed token lists the old one first, muted, with its reason", () => {
    const v = runTokenView(
      [tok({ created_at: at(15, 2), valid_to: at(23, 2) }), tok({ revoked_at: at(15, 2), revoke_reason: "renewal" })],
      false,
    );
    expect(v.lines).toEqual([
      { text: "Azure DevOps token: created 09:02 · expires 17:02 · revoked 15:02 (renewed)", old: true },
      { text: "Azure DevOps token: created 15:02 · expires 23:02", old: false },
    ]);
  });
  it("6c: a paused run says its token was revoked", () => {
    const v = runTokenView([tok({ revoked_at: at(11, 30), revoke_reason: "pause" })], true);
    expect(v.lines[0]).toEqual({ text: "Azure DevOps token: created 09:02 · expires 17:02 · revoked 11:30 (paused)", old: true });
    expect(v.paused).toBe(true);
  });
  it("6d: the last token of a finished run stays plain, each reason reads as its words", () => {
    const reasons: Record<string, string> = {
      run_end: "run ended",
      kill: "run stopped",
      pause: "paused",
      renewal: "renewed",
      widen: "access added",
      drift: "access changed",
      disconnect: "disconnected",
      sweep: "cleaned up after a restart",
    };
    for (const [wire, words] of Object.entries(reasons)) {
      const v = runTokenView([tok({ revoked_at: at(9, 41), revoke_reason: wire })], false);
      expect(v.lines[0]).toEqual({ text: `Azure DevOps token: created 09:02 · expires 17:02 · revoked 09:41 (${words})`, old: false });
    }
  });
  it("6e: a failed renewal names when the live token stops working", () => {
    expect(runTokenView([tok({ renewal_failed: true })], false).renewalFailedAt).toBe("17:02");
    expect(runTokenView([tok({ renewal_failed: true, revoked_at: at(10, 0), revoke_reason: "run_end" })], false).renewalFailedAt).toBeNull();
  });
  it("6f: a revoke that was abandoned names when the token expires on its own", () => {
    expect(runTokenView([tok({ revoke_failed: true })], false).revokeFailedAt).toEqual(["17:02"]);
  });
  it("7: a widening adds an 'Access added' line naming the capability", () => {
    const v = runTokenView(
      [
        tok({ created_at: at(9, 20), valid_to: at(17, 20), added_capabilities: ["pr"] }),
        tok({ revoked_at: at(9, 21), revoke_reason: "widen" }),
      ],
      false,
    );
    expect(v.lines.map((l) => l.text)).toEqual([
      "Azure DevOps token: created 09:02 · expires 17:02 · revoked 09:21 (access added)",
      "Azure DevOps token: created 09:20 · expires 17:20",
    ]);
    expect(v.added).toHaveLength(1);
    expect(v.added[0]).toMatch(/^Access added 09:20: .+ \(new token\)$/);
  });
  it("lists oldest first whatever order the wire sends", () => {
    const v = runTokenView([tok({ created_at: at(15, 2), valid_to: at(23, 2) }), tok({ revoked_at: at(15, 2), revoke_reason: "renewal" })], false);
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
