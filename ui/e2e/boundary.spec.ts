/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Policies -> Wardyn boundary against the real server: a security admin reaches the page from Policies, the
// stored no_limits boundary reads as the off state, a hard-limits draft goes through the dry run before any
// write, and the server's hard_limits gate (409 boundary_hard_limits_gated) is shown in its own words. A
// reload shows nothing was written.
import { test, expect, asRealSecurityAdmin, gotoConsole, navTo } from "./fixtures";
import { BOUNDARY, BOUNDARY_DRY_RUN } from "../src/app/lib/boundary-copy";
import { BASELINE } from "../src/app/lib/governance-copy";

test.describe("Policies: the Wardyn boundary", () => {
  test("a hard-limits draft shows the dry run first, and the gated switch-on is refused in the server's words", async ({ page }) => {
    await asRealSecurityAdmin(page);
    await gotoConsole(page, "admin");
    await navTo(page, "Policies");
    await page.getByRole("link", { name: BOUNDARY.TITLE, exact: true }).click();
    await expect(page).toHaveURL(/\/admin\/policies\/boundary$/);
    await expect(page.getByRole("heading", { level: 1, name: BOUNDARY.TITLE })).toBeVisible();
    await expect(page.getByTestId("boundary-org").getByText(BOUNDARY.ORG_PANEL)).toBeVisible();
    await expect(page.getByTestId("boundary-mode-chip")).toHaveText(BOUNDARY.MODE_ORG);

    await page.getByRole("radio", { name: new RegExp(BOUNDARY.MODE_HARD_BODY.slice(0, 20)) }).check();
    const hard = page.getByTestId("boundary-hard-limits");
    await expect(hard.getByText(BOUNDARY.HARD_HEADING)).toBeVisible();
    await hard.getByLabel(BOUNDARY.BLOCKED_LABEL, { exact: true }).fill("paste.e2e-boundary.example");
    await page.getByRole("button", { name: BOUNDARY.REVIEW, exact: true }).click();

    const report = page.getByTestId("boundary-dry-run");
    await expect(report.getByText(BOUNDARY_DRY_RUN.TITLE)).toBeVisible();
    await expect(report.getByText(BOUNDARY_DRY_RUN.COMPARED)).toBeVisible();
    const submit = report.getByRole("button", { name: BOUNDARY_DRY_RUN.SUBMIT, exact: true });
    await expect(submit).toBeEnabled();
    await submit.click();
    await expect(report.getByText(BASELINE.REFUSED_TITLE)).toBeVisible();
    await expect(report.getByText(/hard_limits cannot be set yet/)).toBeVisible();

    await page.reload();
    await expect(page.getByTestId("boundary-org").getByText(BOUNDARY.ORG_PANEL)).toBeVisible();
  });
});
