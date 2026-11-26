/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, gotoConsole, navTo, asRealSecurityAdmin } from "./fixtures";
import { APPROVALS_NEEDED } from "../src/app/lib/approvals-needed-copy";

// A security admin opens Approvals needed and reads the deploy-time minimum. The e2e daemon sets no minimum, so the
// tab is the read-only state: the table can be read and not changed, and the sentence says what to set to require more.
test.describe("governance — approvals needed", () => {
  test("a security admin sees the minimum row, and with no minimum set the table is read-only", async ({ page }) => {
    await asRealSecurityAdmin(page);
    await gotoConsole(page, "admin");
    await navTo(page, "Governance");
    await page.getByRole("tab", { name: APPROVALS_NEEDED.TITLE, exact: true }).click();

    await expect(page.getByRole("heading", { name: APPROVALS_NEEDED.TITLE, exact: true })).toBeVisible();
    await expect(page.getByText(APPROVALS_NEEDED.MIN_LABEL, { exact: true })).toBeVisible();
    await expect(page.getByText(APPROVALS_NEEDED.MIN_HINT, { exact: true })).toBeVisible();
    await expect(page.getByRole("spinbutton", { name: APPROVALS_NEEDED.MIN_LABEL, exact: true })).toHaveValue("0");

    await expect(page.getByTestId("approvals-minimum-zero")).toContainText("Pending changes keep their existing approval requirements.");
    await expect(page.getByTestId("approvals-minimum-zero").locator("code")).toHaveText("WARDYN_GOVERNANCE_MIN_APPROVALS");
    await expect(page.getByTestId("approvals-default-row").getByRole("spinbutton")).toBeDisabled();
    await expect(page.getByRole("button", { name: APPROVALS_NEEDED.SAVE, exact: true })).toHaveCount(0);
  });
});
