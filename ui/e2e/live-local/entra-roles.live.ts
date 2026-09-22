/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * LL1 — ROLES THROUGH THE REAL ENTRA TENANT (docs/LIVE-TESTS.md).
 *
 * Each identity signs in by redirect: the context starts from a storage state
 * the owner recorded once for that identity, with Wardyn's own cookies removed,
 * so the only thing that can sign it in is the Entra session riding the OIDC
 * redirect. The harness never types a password. If Entra asks for one, the
 * recorded session has lapsed and the case skips with the re-record step.
 *
 * Asserts: the admin sees the admin nav, a member does not, and an identity
 * with no Wardyn role is refused with the no-role message.
 */

import fs from "node:fs";
import {
  test,
  expect,
  type Browser,
  type BrowserContextOptions,
  type Page,
} from "@playwright/test";
import { SIGNIN } from "../../src/app/lib/people-access-copy";
import { identities, missing, whereIs } from "./harness";

const BASE = process.env.WARDYN_LIVE_BASE_URL || "";
const why = missing("WARDYN_LIVE_ENTRA", [
  "WARDYN_LIVE_BASE_URL",
  "WARDYN_LIVE_IDENTITIES_FILE",
]);
test.skip(why !== null, why ?? "");

/** The recorded state minus every cookie and origin that belongs to Wardyn. */
type StorageState = Exclude<
  BrowserContextOptions["storageState"],
  string | undefined
>;

function idpOnlyState(file: string): StorageState {
  const host = new URL(BASE).hostname;
  const state = JSON.parse(fs.readFileSync(file, "utf8")) as StorageState;
  return {
    cookies: state.cookies.filter((c) => c.domain.replace(/^\./, "") !== host),
    origins: state.origins.filter((o) => new URL(o.origin).hostname !== host),
  };
}

const RECORD_HINT = (role: string) =>
  `live: the recorded Entra session for "${role}" is missing or has lapsed. Re-record it: ` +
  `docs/LIVE-TESTS.md, "Record a storage state per identity".`;

/** Signs `role` in by redirect and returns the page, or skips with the re-record step. */
async function signIn(browser: Browser, role: string): Promise<Page> {
  const file = identities()[role]?.storage_state;
  test.skip(!file || !fs.existsSync(file), RECORD_HINT(role));
  const ctx = await browser.newContext({ storageState: idpOnlyState(file!) });
  const page = await ctx.newPage();
  await page.goto(new URL("/auth/login", BASE).toString());
  // Back on Wardyn means Entra answered from its session; still on Entra after
  // the wait means it wants a password, which this harness never types.
  const deadline = Date.now() + 60_000;
  while (whereIs(page, BASE) !== "wardyn" && Date.now() < deadline)
    await page.waitForTimeout(1_000);
  test.skip(whereIs(page, BASE) !== "wardyn", RECORD_HINT(role));
  await page.waitForLoadState("networkidle");
  return page;
}

async function meStatus(
  page: Page,
): Promise<{ status: number; operator?: boolean }> {
  return page.evaluate(async () => {
    const r = await fetch("/api/v1/me", { credentials: "include" });
    const body = r.ok ? ((await r.json()) as { operator?: boolean }) : {};
    return { status: r.status, operator: body.operator };
  });
}

test("the admin signs in by redirect and sees the admin nav", async ({
  browser,
}) => {
  const page = await signIn(browser, "admin");
  expect(
    (await meStatus(page)).operator,
    "the admin identity did not resolve as an operator",
  ).toBe(true);
  await page.goto(new URL("/runs", BASE).toString());
  await expect(page.locator('nav a[href="/permissions"]')).toBeVisible();
});

test("a member signs in by redirect and does not see the admin nav", async ({
  browser,
}) => {
  const page = await signIn(browser, "member");
  const me = await meStatus(page);
  expect(me.status, "the member was not signed in").toBe(200);
  expect(me.operator, "the member identity resolved as an operator").toBe(
    false,
  );
  await page.goto(new URL("/runs", BASE).toString());
  await expect(page.locator('nav a[href="/runs"]')).toBeVisible();
  await expect(page.locator('nav a[href="/permissions"]')).toHaveCount(0);
});

test("an identity with no Wardyn role is refused", async ({ browser }) => {
  const page = await signIn(browser, "norole");
  await expect(page.getByText(SIGNIN.NO_ROLE)).toBeVisible();
  expect(
    (await meStatus(page)).status,
    "the no-role identity got a session",
  ).not.toBe(200);
});
