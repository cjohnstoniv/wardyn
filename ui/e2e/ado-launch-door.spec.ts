/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New Run's Azure DevOps launch door (#386) — the git_credential twin of
// new-run-review-launch.spec.ts's model_credential launch-door case. The
// hermetic e2e backend runs no Entra tenant (scripts/e2e-backend.sh), so both
// the 422 and the connection landing "live" are forced at the route level.
// The create is refused from the Name dialog on Review (reached through the
// run document path); the door opens over it and nothing relaunches.
import { test, expect, gotoConsole, navToRoute } from "./fixtures";
import { ADO } from "../src/app/lib/ado-entra-copy";
import { NR_DOCUMENT } from "../src/app/components/wardyn/copy/new-run/document";
import { NR_LAUNCH } from "../src/app/components/wardyn/copy/new-run/launch";
import { NR_REVIEW } from "../src/app/components/wardyn/copy/new-run/review";
import { NR_START } from "../src/app/components/wardyn/copy/new-run/start";
import { SCENARIO_PREFLIGHT } from "../src/app/lib/new-run-fixtures/scenarios/preflight";
import type { Page } from "@playwright/test";

const DOCUMENT = "api_version: wardyn/v1\nkind: Run\nintent:\n  shape: environment\n  experience: interactive\n  no_repositories_or_drives: true\n";
const nameDialog = (page: Page) => page.getByRole("dialog", { name: NR_LAUNCH.TITLE });
const adoDialog = (page: Page) =>
  page.locator('[role="dialog"], [role="alertdialog"]').filter({ has: page.getByRole("heading", { name: ADO.LAUNCH_DIALOG_TITLE }) });

/** Start, the document path to Review, then the Name dialog with a typed title. */
async function openNameDialog(page: Page, title: string) {
  await page.route("**/api/v1/runs/preflight", (route) => route.fulfill({ json: SCENARIO_PREFLIGHT.member }));
  await gotoConsole(page);
  await navToRoute(page, "/runs/new");
  await page.getByRole("button", { name: NR_START.DOC_YAML, exact: true }).click();
  await page.getByRole("textbox", { name: NR_DOCUMENT.EDITOR_ARIA("YAML") }).fill(DOCUMENT);
  await page.getByRole("button", { name: NR_DOCUMENT.CONTINUE_REVIEW, exact: true }).click();
  await expect(page.getByRole("heading", { name: NR_REVIEW.TITLE })).toBeVisible();
  await expect(page.getByRole("button", { name: NR_REVIEW.LAUNCH_RUN })).toBeEnabled();
  await page.getByRole("button", { name: NR_REVIEW.LAUNCH_RUN }).click();
  await nameDialog(page).getByLabel(NR_LAUNCH.TITLE_LABEL).fill(title);
}

const launchInDialog = (page: Page) => nameDialog(page).getByRole("button", { name: NR_LAUNCH.LAUNCH, exact: true }).click();

test.describe("New Run — the Azure DevOps launch door", () => {
  test("launch -> 422 -> dialog names the org -> connect -> toast -> form intact, not relaunched", async ({
    page,
    context,
  }) => {
    const org = "https://dev.azure.com/contoso";

    // The launch-time refusal (runs.go's gitCredentialRefusal): the ONLY
    // door this case exercises is POST /api/v1/runs, mirroring new-run-review-launch.spec.ts's
    // own create-refusal stubs.
    let creates = 0;
    await page.route("**/api/v1/runs", async (route) => {
      if (route.request().method() !== "POST") return route.fallback();
      creates++;
      await route.fulfill({
        status: 422,
        contentType: "application/json",
        body: JSON.stringify({
          error: "you are not connected to Azure DevOps — connect and start the run again",
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
        // The live row is the refusal's own org: only that organisation's sign-in finishes this connect (DR-0270).
        body: JSON.stringify(live ? [{ state: "live", source: "separate", org, kind: "azure_devops" }] : []),
      });
    });

    await openNameDialog(page, "e2e ado launch door");
    await launchInDialog(page);

    // The dialog opened itself, with no click, and names the org from the
    // 422 body (review finding F1).
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

    // ...but review finding F8: NOTHING relaunches. The Name dialog is exactly
    // as it stood, over Review, ready for the person to press Launch.
    await expect(page).not.toHaveURL(/launched=/);
    expect(creates).toBe(1);
    await expect(nameDialog(page).getByLabel(NR_LAUNCH.TITLE_LABEL)).toHaveValue("e2e ado launch door");
    await expect(nameDialog(page).getByRole("button", { name: NR_LAUNCH.LAUNCH, exact: true })).toBeVisible();
  });

  test("Cancel closes the dialog without ever opening a popup", async ({ page, context }) => {
    await page.route("**/api/v1/runs", async (route) => {
      if (route.request().method() !== "POST") return route.fallback();
      await route.fulfill({
        status: 422,
        contentType: "application/json",
        body: JSON.stringify({
          error: "you are not connected to Azure DevOps — connect and start the run again",
          reason: "git_credential",
          org: "https://dev.azure.com/contoso",
        }),
      });
    });

    await openNameDialog(page, "e2e ado cancel");
    await launchInDialog(page);
    await expect(page.getByRole("heading", { name: ADO.LAUNCH_DIALOG_TITLE })).toBeVisible();

    let popupOpened = false;
    context.on("page", () => {
      popupOpened = true;
    });
    await adoDialog(page).getByRole("button", { name: "Cancel" }).click();
    await expect(page.getByRole("heading", { name: ADO.LAUNCH_DIALOG_TITLE })).toHaveCount(0);
    expect(popupOpened).toBe(false);
  });

  // #2621: a refusal whose cause is an expired own token cannot be repaired by
  // Connect, so no door opens; the server's sentence stays under Launch.
  test("an expired-token refusal opens no Connect dialog and leaves the sentence under Launch", async ({
    page,
    context,
  }) => {
    const sentence = "your Azure DevOps token has expired: replace it in Settings, then start the run again";
    await page.route("**/api/v1/runs", async (route) => {
      if (route.request().method() !== "POST") return route.fallback();
      await route.fulfill({
        status: 422,
        contentType: "application/json",
        body: JSON.stringify({
          error: sentence,
          reason: "git_credential",
          org: "https://dev.azure.com/contoso",
          cause: "token_expired",
        }),
      });
    });

    await openNameDialog(page, "e2e ado expired token");
    let popupOpened = false;
    context.on("page", () => {
      popupOpened = true;
    });
    await launchInDialog(page);

    await expect(nameDialog(page).locator("#name-run-sentence")).toContainText(sentence);
    await expect(page.getByRole("heading", { name: ADO.LAUNCH_DIALOG_TITLE })).toHaveCount(0);
    expect(popupOpened).toBe(false);
  });
});
