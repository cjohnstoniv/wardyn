/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The New Run rail's Azure DevOps launch door (#386) — the git_credential
// twin of new-run.spec.ts's model_credential launch-door case. The hermetic
// e2e backend runs no Entra tenant (scripts/e2e-backend.sh), so both the 422
// and the connection landing "live" are forced at the route level, the way
// new-run.spec.ts's own refuseLaunch/perUserRow helpers force model_credential.
import { test, expect, gotoConsole } from "./fixtures";
import { ADO } from "../src/app/lib/ado-entra-copy";
import type { Page } from "@playwright/test";

async function openNewRun(page: Page) {
  await gotoConsole(page);
  await page.getByRole("button", { name: "New run" }).click();
  await expect(page).toHaveURL(/\/runs\/new$/);
  await expect(page.getByRole("heading", { name: "New run" })).toBeVisible();
}

test.describe("New run rail — the Azure DevOps launch door", () => {
  test("launch -> 422 -> dialog names the org -> connect -> toast -> form intact, not relaunched", async ({
    page,
    context,
  }) => {
    const org = "https://dev.azure.com/contoso";

    // The launch-time refusal (runs.go's gitCredentialRefusal): the ONLY
    // door this case exercises is POST /api/v1/runs, mirroring new-run.spec.ts's
    // own refuseLaunch helper.
    await page.route("**/api/v1/runs", async (route) => {
      if (route.request().method() !== "POST") return route.fallback();
      await route.fulfill({
        status: 422,
        contentType: "application/json",
        body: JSON.stringify({
          error: "git_credential: you are not connected to Azure DevOps — connect and start the run again",
          reason: "git_credential",
          org,
        }),
      });
    });

    // The popup-driven connect polls this route (use-ado-connect.ts) — it
    // never trusts the popup's own page, so the popup navigating to the
    // real (unconfigured, 404) sign-in door in this hermetic backend is
    // irrelevant to the poll; only this route decides when it reads "live".
    let live = false;
    await page.route("**/api/v1/me/scm-access", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(live ? [{ state: "live", source: "separate" }] : []),
      });
    });

    await openNewRun(page);
    await page.getByLabel("Title").fill("e2e ado launch door");
    await page.getByRole("button", { name: "Launch run" }).click();

    // The dialog opened itself, with no click, and names the org from the
    // 422 body (review finding F1) — no preflight verdict has run yet.
    await expect(page.getByRole("heading", { name: ADO.LAUNCH_DIALOG_TITLE })).toBeVisible();
    await expect(page.getByText(ADO.LAUNCH_DIALOG_BODY(org))).toBeVisible();

    const [popup] = await Promise.all([
      context.waitForEvent("page"),
      page.getByRole("button", { name: ADO.CONNECT_CTA }).click(),
    ]);
    // The next poll tick reads live; the hook closes the popup ITSELF
    // (review finding F9) — nothing in the popup's own page does this.
    live = true;
    await popup.waitForEvent("close");

    // The dialog closes and the toast lands...
    await expect(page.getByRole("heading", { name: ADO.LAUNCH_DIALOG_TITLE })).toHaveCount(0);
    await expect(page.getByText(ADO.RELAUNCH_TOAST_TITLE)).toBeVisible();
    await expect(page.getByText(ADO.RELAUNCH_TOAST_BODY)).toBeVisible();

    // ...but review finding F8: NOTHING relaunches. The form is exactly as
    // it stood, still on /runs/new, ready for the person to press Launch.
    await expect(page).toHaveURL(/\/runs\/new$/);
    await expect(page.getByLabel("Title")).toHaveValue("e2e ado launch door");
    await expect(page.getByRole("button", { name: "Launch run" })).toBeVisible();
  });

  test("Cancel closes the dialog without ever opening a popup", async ({ page, context }) => {
    await page.route("**/api/v1/runs", async (route) => {
      if (route.request().method() !== "POST") return route.fallback();
      await route.fulfill({
        status: 422,
        contentType: "application/json",
        body: JSON.stringify({
          error: "git_credential: you are not connected to Azure DevOps — connect and start the run again",
          reason: "git_credential",
          org: "https://dev.azure.com/contoso",
        }),
      });
    });

    await openNewRun(page);
    await page.getByLabel("Title").fill("e2e ado cancel");
    await page.getByRole("button", { name: "Launch run" }).click();
    await expect(page.getByRole("heading", { name: ADO.LAUNCH_DIALOG_TITLE })).toBeVisible();

    let popupOpened = false;
    context.on("page", () => {
      popupOpened = true;
    });
    await page.getByRole("button", { name: "Cancel" }).click();
    await expect(page.getByRole("heading", { name: ADO.LAUNCH_DIALOG_TITLE })).toHaveCount(0);
    expect(popupOpened).toBe(false);
  });
});
