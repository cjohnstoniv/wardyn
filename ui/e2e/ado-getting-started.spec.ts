/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { Page } from "@playwright/test";
import { test, expect, gotoConsole, mockMemberRole, navToRoute } from "./fixtures";
import { ADO } from "../src/app/lib/ado-entra-copy";
import { ADO_PAT } from "../src/app/lib/ado-pat-copy";
import { formatDay } from "../src/app/lib/ado-pat-display";
import { aheadByHours } from "../src/app/lib/test-clock";

// The Azure DevOps chip on Getting started (#386) — the hermetic e2e backend
// runs no Entra tenant, so every state is forced by intercepting
// /setup/status the way setup-gate.spec.ts does: fetch the REAL response,
// splice in `scm_access`, fulfill. Every other field stays what the daemon
// actually serves.
async function mockScmAccess(page: Page, scm_access: Record<string, unknown>): Promise<void> {
  // Cache-and-serve, not route.fetch()+refulfill per match: the landing
  // redirect (gotoConsole) and the setup screen's own mount both hit
  // /setup/status, and a real round trip PER match raced Playwright disposing
  // an in-flight route's response at teardown ("apiResponse.json: Response
  // has been disposed").
  let cached: Record<string, unknown> | null = null;
  await page.route("**/api/v1/setup/status*", async (route) => {
    if (!cached) {
      const response = await route.fetch();
      const json = await response.json();
      json.scm_access = scm_access;
      cached = json;
    }
    await route.fulfill({ json: cached! });
  });
}

test.describe("Getting started — the Azure DevOps chip (mocked /setup/status.scm_access)", () => {
  test.beforeEach(async ({ page }) => {
    await mockMemberRole(page);
  });

  test("no Azure DevOps row configured: no chip at all", async ({ page }) => {
    await gotoConsole(page);
    await navToRoute(page, "/setup");
    await expect(page.getByRole("heading", { name: "What's set up for you" })).toBeVisible();
    await expect(page.getByText(ADO.ACCESS_LIVE_ORG)).toHaveCount(0);
    await expect(page.getByText(ADO.ACCESS_NOT_CONNECTED)).toHaveCount(0);
  });

  test("live through the organisation's sign-in: success chip, no action line, no button", async ({ page }) => {
    await mockScmAccess(page, { state: "live", source: "org" });
    await gotoConsole(page);
    await navToRoute(page, "/setup");
    await expect(page.getByText(ADO.ACCESS_LIVE_ORG)).toBeVisible();
    await expect(page.getByRole("button", { name: ADO.CONNECT_ADO })).toHaveCount(0);
  });

  test("not connected: warning chip, the row-is-newer cause, and Connect Azure DevOps", async ({ page }) => {
    await mockScmAccess(page, { state: "not_configured", cause: "row_is_newer" });
    await gotoConsole(page);
    await navToRoute(page, "/setup");
    await expect(page.getByText(ADO.ACCESS_NOT_CONNECTED)).toBeVisible();
    await expect(page.getByText(ADO.CAUSE_ROW_IS_NEWER)).toBeVisible();
    await expect(page.getByRole("button", { name: ADO.CONNECT_ADO })).toBeVisible();
  });

  test("the admin's shared credential expired: warning chip, the admin-facing line, no button", async ({ page }) => {
    await mockScmAccess(page, { state: "shared_expired" });
    await gotoConsole(page);
    await navToRoute(page, "/setup");
    await expect(page.getByText(ADO.ACCESS_SHARED_EXPIRED)).toBeVisible();
    await expect(page.getByText(ADO.ACCESS_SHARED_EXPIRED_ACTION)).toBeVisible();
    await expect(page.getByRole("button", { name: ADO.CONNECT_ADO })).toHaveCount(0);
  });

  // The own-token chip packet: a refused own token is red and offers Replace token,
  // which opens the add-token dialog, never the Entra sign-in popup.
  test("a refused own token: danger chip, the refusal line, and Replace token opens the add-token dialog", async ({ page }) => {
    const expiresOn = aheadByHours(20 * 24).slice(0, 10);
    const refusedAt = aheadByHours(-48);
    await mockScmAccess(page, {
      state: "live",
      source: "own",
      token_mode: "own_pat",
      org: "https://dev.azure.com/wardyn-live-test",
      max_days: 30,
      expires_on: expiresOn,
      refused_at: refusedAt,
    });
    await gotoConsole(page);
    await navToRoute(page, "/setup");
    await expect(page.getByText("Azure DevOps · Refused")).toBeVisible();
    await expect(page.getByText(ADO.ACCESS_SHARED_LIVE)).toHaveCount(0);
    await expect(page.getByText(ADO_PAT.OWN_REFUSED_LINE(formatDay(refusedAt), formatDay(expiresOn)))).toBeVisible();
    await expect(page.getByRole("button", { name: ADO.CONNECT_ADO })).toHaveCount(0);
    await page.getByRole("button", { name: ADO_PAT.OWN_REPLACE }).click();
    await expect(page.getByRole("dialog", { name: ADO_PAT.OWN_DIALOG_TITLE })).toBeVisible();
  });

  test("Connect Azure DevOps opens the connect popup (the daemon's own redirect door)", async ({ page, context }) => {
    await mockScmAccess(page, { state: "not_configured", cause: "row_is_newer" });
    await gotoConsole(page);
    await navToRoute(page, "/setup");
    const [popup] = await Promise.all([
      context.waitForEvent("page"),
      page.getByRole("button", { name: ADO.CONNECT_ADO }).click(),
    ]);
    await expect(popup).toHaveURL(/\/api\/v1\/scm\/azure-devops\/signin/);
    await popup.close();
  });
});
