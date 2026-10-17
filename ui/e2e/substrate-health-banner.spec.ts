/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { Page } from "@playwright/test";

import { test, expect, mockMemberRole } from "./fixtures";
import { SUBSTRATE_BANNER } from "../src/app/lib/substrate-banner-copy";

// M10 — the substrate_health band. The seeded backend has a healthy runner, so
// the failing row is spliced onto GET /setup/status; what this proves is what
// only a browser can: the band is in the Admin view's shell for an admin, shows
// the row's detail verbatim, its action opens the Review step, and a member
// never sees it. The REAL fault (runner RoleBinding deleted on kind) is walked
// live.
const DETAIL = "The sandbox runner refuses Wardyn's credentials, so new runs can't start.";

async function stubSubstrateRow(page: Page, row: Record<string, unknown> | null): Promise<void> {
  // Cache-and-serve, not route.fetch()+refulfill per match (fixtures.ts#mockMemberRole): the shell
  // polls /setup/status, and a real round trip per match raced Playwright disposing an in-flight
  // route's response at teardown ("apiResponse.json: Response has been disposed").
  let cached: Record<string, unknown> | null = null;
  await page.route("**/api/v1/setup/status*", async (route) => {
    if (!cached) {
      const json = (await (await route.fetch()).json()) as { checks?: Array<{ id: string }> } & Record<string, unknown>;
      json.checks = [...(json.checks ?? []).filter((c) => c.id !== "substrate_health"), ...(row ? [row as { id: string }] : [])];
      cached = json;
    }
    await route.fulfill({ json: cached! });
  });
}

const FAILING = { id: "substrate_health", label: "Runner health", status: "fail", cause: "runner_auth", detail: DETAIL };

test.describe("the substrate_health band", () => {
  test("an admin sees it, with the row's detail, and its action opens the Review step", async ({ page }) => {
    await stubSubstrateRow(page, FAILING);
    await page.goto("/admin/setup?step=environment");
    await expect(page.getByText(SUBSTRATE_BANNER.TITLE_AUTH, { exact: true })).toBeVisible();
    await expect(page.getByText(DETAIL, { exact: true })).toBeVisible();
    await page.getByRole("button", { name: SUBSTRATE_BANNER.ACTION }).click();
    await expect(page).toHaveURL(/step=review/);
  });

  test("an admin sees no band while the row is ok", async ({ page }) => {
    await stubSubstrateRow(page, { ...FAILING, status: "ok", cause: undefined });
    await page.goto("/admin/setup?step=environment");
    await expect(page.getByRole("heading").first()).toBeVisible();
    await expect(page.getByText(SUBSTRATE_BANNER.TITLE_AUTH, { exact: true })).toHaveCount(0);
  });

  test("a member never sees it", async ({ page }) => {
    await mockMemberRole(page);
    await stubSubstrateRow(page, FAILING);
    await page.goto("/runs");
    await expect(page.getByRole("heading").first()).toBeVisible();
    await expect(page.getByText(SUBSTRATE_BANNER.TITLE_AUTH, { exact: true })).toHaveCount(0);
    await expect(page.getByRole("button", { name: SUBSTRATE_BANNER.ACTION })).toHaveCount(0);
  });
});
