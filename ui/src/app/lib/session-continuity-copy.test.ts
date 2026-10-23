/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { resolve } from "node:path";
import { describe, expect, it } from "vitest";
import { noticeView, returnRunOf, SESSION_ENDED, SESSION_MENU, signedInUntil, TERMINAL_SESSION } from "./session-continuity-copy";
import { SESSION_ENDED_REASON } from "./api/core";
import { parseFrozenTables, splitKey } from "./copy-doc-parity";

// docs/design/session-continuity-canon.md's "Frozen strings" table, parsed back and compared with the
// module, as the sign-in and unsaved-guard canons are (copy-doc-parity.ts).
const doc = parseFrozenTables(resolve(process.cwd(), "../docs/design/session-continuity-canon.md"), /^## Frozen strings/);
const MODULES: Record<string, Record<string, unknown>> = { SESSION_ENDED, SESSION_MENU, TERMINAL_SESSION };

function render(docKey: string): string {
  const [ns, rest] = [docKey.slice(0, docKey.indexOf(".")), docKey.slice(docKey.indexOf(".") + 1)];
  const [name, args] = splitKey(rest);
  const value = MODULES[ns]?.[name];
  if (value === undefined) throw new Error(`${docKey}: no such key`);
  return typeof value === "function" ? (value as (...a: string[]) => string)(...args.map((a) => `{${a}}`)) : String(value);
}

describe("session-continuity-copy — session-continuity-canon.md, #1995", () => {
  it("finds all 14 frozen keys in the doc", () => {
    expect(doc.size).toBe(14);
  });

  it.each([...doc.keys()])("%s is byte-exact", (key) => {
    expect(render(key)).toBe(doc.get(key));
  });

  it("covers every module key, and freezes none the module lacks", () => {
    const names = Object.entries(MODULES)
      .flatMap(([ns, m]) => Object.keys(m).map((k) => `${ns}.${k}`))
      .sort();
    expect([...doc.keys()].map((k) => k.replace(/\(.*\)$/, "")).sort()).toEqual(names);
  });
});

describe("noticeView — one notice per reason", () => {
  it.each([
    ["expired_session", SESSION_ENDED.EXPIRED, "warning"],
    ["revoked_session", SESSION_ENDED.REVOKED, "warning"],
    ["role_stamp_stale", SESSION_ENDED.ROLE_STALE, "warning"],
    ["invalid_session", SESSION_ENDED.INVALID, "warning"],
    ["identity_deactivated", SESSION_ENDED.DEACTIVATED, "danger"],
  ])("%s", (reason, text, tone) => {
    expect(noticeView({ kind: "reason", reason })).toMatchObject({ lines: [text], tone });
  });

  it("only a deactivated person gets the quieter Sign in", () => {
    expect(noticeView({ kind: "reason", reason: "identity_deactivated" }).quietSignIn).toBe(true);
    expect(noticeView({ kind: "reason", reason: "expired_session" }).quietSignIn).toBe(false);
  });

  it("an unknown reason and a refused stored token keep today's sentence", () => {
    expect(noticeView({ kind: "reason", reason: "something_new" }).lines).toEqual([SESSION_ENDED_REASON]);
    expect(noticeView({ kind: "fallback" }).lines).toEqual([SESSION_ENDED_REASON]);
  });

  it("another tab, and this tab's own sign-out with and without SSO", () => {
    expect(noticeView({ kind: "elsewhere" }).lines).toEqual([SESSION_ENDED.ELSEWHERE]);
    expect(noticeView({ kind: "own", sso: true }).lines).toEqual([SESSION_ENDED.OWN, SESSION_ENDED.OWN_SHARED]);
    expect(noticeView({ kind: "own", sso: false }).lines).toEqual([SESSION_ENDED.OWN]);
  });
});

describe("returnRunOf", () => {
  it("names a run's page by its short id, and nothing else", () => {
    expect(returnRunOf("/runs/run_7f3c9a41-1111-2222-3333-444455556666")).toBe("7f3c9a41");
    expect(returnRunOf("/admin/runs/7f3c9a41aa?tab=audit")).toBe("7f3c9a41");
    expect(returnRunOf("/runs/new")).toBeNull();
    expect(returnRunOf("/runs")).toBeNull();
    expect(returnRunOf("/setup")).toBeNull();
  });
});

describe("signedInUntil", () => {
  const now = new Date(2026, 9, 21, 9, 0);
  it("says today for an expiry on the same day", () => {
    expect(signedInUntil(new Date(2026, 9, 21, 18, 40), now)).toMatch(/^Signed in until .*:40.* today$/);
  });
  it("names the day for a later one", () => {
    expect(signedInUntil(new Date(2026, 9, 22, 6, 5), now)).toMatch(/^Signed in until .* on .*22/);
  });
});
