/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, gotoConsole, mockMemberRole, navToRoute } from "./fixtures";
import { ADO } from "../src/app/lib/ado-entra-copy";

// The Azure DevOps connect door (#628). It keeps its flow — a popup opened on
// the click and navigated straight to the daemon's own redirect, with no
// sign-in sandbox and so no progress to draw. The one state the packet draws
// is the popup-blocked fallback: a sentence and a button-shaped link, which the
// browser itself opens, so no about:blank page is ever involved.

const SIGNIN_PATH = "/api/v1/scm/azure-devops/signin";

test.describe("the Azure DevOps connect door (#628)", () => {
  test("a blocked connect popup offers the sign-in as a button, which opens the sign-in itself", async ({
    page,
    context,
  }) => {
    await mockMemberRole(page);
    await page.route("**/api/v1/setup/status*", async (route) => {
      const response = await route.fetch();
      const json = await response.json();
      json.scm_access = { state: "not_configured", cause: "row_is_newer" };
      await route.fulfill({ response, json });
    });
    await context.route(`**${SIGNIN_PATH}`, (route) =>
      route.fulfill({ contentType: "text/html", body: "<title>Microsoft sign-in</title>" }),
    );
    // A strict popup policy: window.open answers null, which is the contract
    // use-ado-connect.ts reads as "blocked".
    await page.addInitScript(() => {
      window.open = () => null;
    });

    await gotoConsole(page);
    await navToRoute(page, "/setup");
    await page.getByRole("button", { name: ADO.CONNECT_ADO }).click();

    await expect(page.getByText(ADO.CONNECT_POPUP_BLOCKED)).toBeVisible();
    const open = page.getByRole("link", { name: ADO.CONNECT_POPUP_OPEN });
    await expect(open).toHaveAttribute("href", SIGNIN_PATH);
    await expect(open).toHaveAttribute("rel", "noopener noreferrer");

    const [tab] = await Promise.all([context.waitForEvent("page"), open.click()]);
    await expect(tab).toHaveURL(new RegExp(`${SIGNIN_PATH}$`));
    expect(await tab.evaluate(() => window.opener)).toBeNull();
    await tab.close();
  });
});
