/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #2446 — Governance, Runner credentials: a stored per-trust exception is drawn read-only under its row and in
// People see, so "People see" is no longer untrue for a trust class. The document is spliced (the console has no
// control that writes an exception) and so is the PUT, whose body is checked for keeping the exception.
import { test, expect, asRealSecurityAdmin, gotoConsole, navTo } from "./fixtures";
import { DELIVERY } from "../src/app/lib/governance-copy";
import { RUNNER_TRUST } from "../src/app/components/wardyn/copy/runner-pools";

const POLICY = {
  classes: { env_secret: { mode: "runner_resident" } },
  by_trust: { personal: { classes: { env_secret: { mode: "refuse" } } } },
};

test.describe("Runner credentials: stored exceptions (#2446)", () => {
  test("the note, the line under the row and the People see line show; a save sends the exception back as stored", async ({ page }) => {
    await page.route("**/api/v1/governance/credential-delivery", (route) => {
      const req = route.request();
      if (req.method() === "GET") {
        return route.fulfill({ json: { policy: POLICY, updated_at: "", updated_by: "" }, headers: { ETag: '"e2e"' } });
      }
      return route.fallback();
    });
    await asRealSecurityAdmin(page);
    await gotoConsole(page, "admin");
    await navTo(page, "Governance");
    await page.getByRole("tab", { name: DELIVERY.TAB }).click();

    await expect(page.getByText(DELIVERY.EXCEPTIONS_NOTE)).toBeVisible();
    const line = page.getByTestId("dc-env_secret-exceptions");
    await expect(line.getByText(DELIVERY.EXCEPTION_CHIP, { exact: true })).toBeVisible();
    await expect(line.getByText(RUNNER_TRUST.PERSONAL, { exact: true })).toBeVisible();
    await expect(line.getByText(DELIVERY.MODE.refuse, { exact: true })).toBeVisible();
    await expect(line.getByText(DELIVERY.EXCEPTION_KEPT, { exact: true })).toBeVisible();
    await expect(
      page.getByText(DELIVERY.EXCEPTION_PEOPLE_SEE(RUNNER_TRUST.PERSONAL, DELIVERY.LOCAL_DELIVERY.refuse), { exact: true }),
    ).toBeVisible();
    // Only the class the exception names has one.
    await expect(page.getByText(DELIVERY.EXCEPTION_CHIP, { exact: true })).toHaveCount(1);

    // Change another row and save: the PUT carries by_trust unchanged.
    let sent: unknown = null;
    await page.route("**/api/v1/governance/credential-delivery", (route) => {
      const req = route.request();
      if (req.method() === "PUT") {
        sent = req.postDataJSON();
        return route.fulfill({ json: { policy: sent, updated_at: "", updated_by: "" }, headers: { ETag: '"e2e2"' } });
      }
      return route.fallback();
    });
    await page.getByRole("radiogroup", { name: DELIVERY.CLASS.api_key }).getByRole("radio", { checked: false, disabled: false }).first().click();
    await page.getByRole("button", { name: DELIVERY.SAVE, exact: true }).click();
    await expect(page.getByText(DELIVERY.SAVED)).toBeVisible();
    expect(sent).toMatchObject({ by_trust: POLICY.by_trust });
  });
});
