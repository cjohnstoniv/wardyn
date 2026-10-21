/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, gotoConsole, navTo } from "./fixtures";

test.describe.configure({ mode: "serial" });

test.describe("Run detail — the Identity widget", () => {
  // #3036: the Run row is a reference: the run's name, then a short id that copies the whole one.
  test("the Run row shows a short copyable id, not the 36-character one", async ({ page }) => {
    await gotoConsole(page);
    await navTo(page, "Runs");
    await page.getByText("e2e fixture 2").click();
    const identity = page.locator("section", { has: page.getByRole("heading", { name: "Identity" }) });
    const ref = identity.getByTestId("run-ref");
    await expect(ref.getByRole("button")).toHaveText(/^[0-9a-f]{8}$/);
    await expect(ref.getByRole("button")).toHaveAttribute("title", /^[0-9a-f-]{36}\s+Click to copy$/);
    await expect(identity.getByText(/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/)).toHaveCount(0);
  });

  test("at 1440x900 with a long task, no floating Focus/Edit button sits over an Identity row", async ({ page }) => {
    await page.setViewportSize({ width: 1440, height: 900 });
    // Settled host with a barrier, as the other cockpit specs splice it: the no-barrier shell banner would
    // take rows from the cockpit and move the geometry under test.
    await page.route("**/api/v1/setup/status*", async (route) => {
      const response = await route.fetch();
      const json = await response.json();
      json.runner = { ...json.runner, driver: "docker", confinement_classes: ["CC1"] };
      await route.fulfill({ response, json });
    });
    await page.route("**/api/v1/runs/*", async (route) => {
      if (route.request().method() !== "GET") return route.fallback();
      const response = await route.fetch();
      const json = await response.json();
      if (json.task === "e2e fixture 2") {
        json.task = "Refactor the billing module so that invoices are generated lazily, add regression tests for the rounding rules, and update the migration notes. Do not touch the public API.";
        json.description = "Quarterly cleanup requested by finance.";
      }
      await route.fulfill({ response, json });
    });
    await gotoConsole(page);
    await navTo(page, "Runs");
    await page.getByText("e2e fixture 2").click();
    await expect(page.getByRole("heading", { name: "Identity" })).toBeVisible();

    // R-U1 (FU091-L2): the Focus and Edit layout buttons used to float over the cockpit's bottom-right corner, where
    // the Identity widget ends, and covered its last rows (Runner, Pool, Started). The controls now have a strip
    // of their own under the canvas.
    const identity = page.locator("section", { has: page.getByRole("heading", { name: "Identity" }) });
    const focus = page.getByRole("button", { name: "Focus", exact: true });
    const edit = page.getByRole("button", { name: "Edit layout", exact: true });
    await expect(focus).toBeVisible();
    await expect(edit).toBeVisible();

    // The tiles scroll inside the cockpit's canvas, so anything floating over the canvas covers whichever tile
    // has scrolled under it. The controls must therefore sit outside the canvas, not over its corner.
    const canvas = identity.locator("xpath=ancestor::div[contains(@class,'react-grid-layout')][1]/..");
    const [pane, ...buttons] = await Promise.all([canvas, focus, edit].map((l) => l.boundingBox()));
    expect(pane).not.toBeNull();
    for (const b of buttons) {
      expect(b).not.toBeNull();
      const apart = b!.y >= pane!.y + pane!.height || b!.y + b!.height <= pane!.y || b!.x >= pane!.x + pane!.width || b!.x + b!.width <= pane!.x;
      expect(apart, `a control floats over the tile canvas: ${JSON.stringify({ pane, b })}`).toBe(true);
    }

  });
});
