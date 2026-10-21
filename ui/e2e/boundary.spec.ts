/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Policies -> Wardyn boundary against the real server: a security admin reaches the page from Policies, the
// stored no_limits boundary reads as the off state, and while the server gates hard_limits the Hard limits
// choice is off with its reason, so the hard-limits list (and its claims) can't be reached and nothing is
// offered for review. No second-admin claim is made before a submit.
import { test, expect, asRealSecurityAdmin, gotoConsole, navTo } from "./fixtures";
import { BOUNDARY } from "../src/app/lib/boundary-copy";

test.describe("Policies: the Wardyn boundary", () => {
  test("the off state is shown, and Hard limits is off with its reason while the server gates it", async ({ page }) => {
    await asRealSecurityAdmin(page);
    await gotoConsole(page, "admin");
    await navTo(page, "Policies");
    // The link sits in the page body, not the header, so the header keeps its one action (#2831).
    await expect(page.getByTestId("page-header-actions").getByRole("link", { name: BOUNDARY.TITLE })).toHaveCount(0);
    await page.getByRole("link", { name: BOUNDARY.TITLE, exact: true }).click();
    await expect(page).toHaveURL(/\/admin\/policies\/boundary$/);
    await expect(page.getByRole("heading", { level: 1, name: BOUNDARY.TITLE })).toBeVisible();
    await expect(page.getByTestId("boundary-org").getByText(BOUNDARY.ORG_PANEL)).toBeVisible();
    await expect(page.getByTestId("boundary-mode-chip")).toHaveText(BOUNDARY.MODE_ORG);

    const hard = page.getByRole("radio", { name: new RegExp(BOUNDARY.MODE_HARD_BODY.slice(0, 20)) });
    await expect(hard).toBeDisabled();
    await expect(hard).toHaveAccessibleDescription(BOUNDARY.HARD_GATED);
    await expect(page.getByTestId("boundary-hard-limits")).toHaveCount(0);
    await expect(page.getByText(BOUNDARY.HARD_HEADING)).toHaveCount(0);
    await expect(page.getByRole("button", { name: BOUNDARY.REVIEW, exact: true })).toBeDisabled();
    await expect(page.getByText(/second admin|another security admin/)).toHaveCount(0);
  });
});
