/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Governance, Runner credentials: a Save that meets "someone else saved first" reloads the stored document and
// keeps the refused draft only in the saved-elsewhere banner. While that banner is up, leaving the screen is
// guarded (the draft would be lost), and the banner says what the screen now shows rather than asking for a
// reload. The PUT is refused here; the GET is the real server's.
import type { Page } from "@playwright/test";
import { test, expect, asRealSecurityAdmin, gotoConsole, navTo } from "./fixtures";
import { FORM_ACTIONS } from "../src/app/components/wardyn/copy/form-actions";
import { DELIVERY } from "../src/app/lib/governance-copy";
import { UNSAVED } from "../src/app/lib/unsaved-copy";
import { PROVIDERS, PROVIDERS_EXTRA } from "../src/app/lib/workspace-providers-copy";

async function refusedSave(page: Page): Promise<void> {
  await page.route("**/api/v1/governance/credential-delivery", (route) =>
    route.request().method() === "PUT" ? route.fulfill({ status: 412, json: { error: "If-Match does not match" } }) : route.fallback(),
  );
  await asRealSecurityAdmin(page);
  await gotoConsole(page, "admin");
  await navTo(page, "Governance");
  await page.getByRole("tab", { name: DELIVERY.TAB }).click();
  // Change one row to a mode it is not on, so there is a draft to refuse.
  await expect(page.getByTestId("form-actions-unsaved")).toHaveText("");
  await page.getByRole("radiogroup", { name: DELIVERY.CLASS.api_key }).getByRole("radio", { checked: false, disabled: false }).first().click();
  // The shared form bar counts the changed row.
  await expect(page.getByTestId("form-actions-unsaved")).toHaveText(FORM_ACTIONS.UNSAVED(1));
  await page.getByRole("button", { name: DELIVERY.SAVE, exact: true }).click();
  await expect(page.getByText(PROVIDERS.SAVED_ELSEWHERE_TITLE)).toBeVisible();
}

test.describe("Runner credentials: a save someone else got in front of", () => {
  test("the refused draft held only in the banner still guards leaving the screen", async ({ page }) => {
    await refusedSave(page);
    await navTo(page, "Audit");
    await expect(page.getByRole("alertdialog").getByText(UNSAVED.TITLE)).toBeVisible();
    await expect(page).toHaveURL(/\/governance/);
  });

  test("the banner says the screen already shows their version, and Dismiss lets the screen go", async ({ page }) => {
    await refusedSave(page);
    await expect(page.getByText(PROVIDERS_EXTRA.SAVED_ELSEWHERE_RELOADED_BODY)).toBeVisible();
    await expect(page.getByRole("button", { name: PROVIDERS_EXTRA.DISCARD_AND_RELOAD })).toHaveCount(0);
    await page.getByRole("button", { name: PROVIDERS_EXTRA.DISCARD_RELOADED, exact: true }).click();
    await expect(page.getByText(PROVIDERS.SAVED_ELSEWHERE_TITLE)).toHaveCount(0);
    await navTo(page, "Audit");
    await expect(page).toHaveURL(/\/audit/);
    await expect(page.getByRole("alertdialog")).toHaveCount(0);
  });
});

test.describe("Runner credentials: one table", () => {
  test("the options are columns, People see is quiet at the default and highlighted when it differs, a bound's reason sits in its cell", async ({ page }) => {
    await asRealSecurityAdmin(page);
    await gotoConsole(page, "admin");
    await navTo(page, "Governance");
    await page.getByRole("tab", { name: DELIVERY.TAB }).click();

    const keys = page.getByRole("radiogroup", { name: DELIVERY.CLASS.api_key });
    await expect(keys).toBeVisible();
    for (const head of [DELIVERY.COL_CREDENTIAL, DELIVERY.MODE.via_org, DELIVERY.MODE.runner_resident, DELIVERY.MODE.refuse, DELIVERY.COL_PEOPLE_SEE]) {
      await expect(page.getByText(head, { exact: true }).first()).toBeVisible();
    }
    await expect(keys.locator("[data-people-see]")).toHaveAttribute("data-people-see", "default");

    await keys.getByRole("radio", { name: DELIVERY.MODE.runner_resident }).click();
    await expect(keys.locator("[data-people-see]")).toHaveAttribute("data-people-see", "differs");
    await expect(keys.locator("[data-people-see]")).toHaveText(DELIVERY.LOCAL_DELIVERY.runner_resident);

    const env = page.getByRole("radiogroup", { name: DELIVERY.CLASS.env_secret });
    await expect(env.getByRole("radio", { name: DELIVERY.MODE.via_org })).toBeDisabled();
    await expect(env.getByText(DELIVERY.BOUND_NO_VIA_ORG)).toBeVisible();
  });
});
