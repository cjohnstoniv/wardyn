/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// 0.9.4 console UX Tier B (admin, settings, governance, workspaces screens): the pins that need a real layout
// engine. Everything text- or class-shaped is pinned in vitest next to its screen.
import type { Locator, Page } from "@playwright/test";
import { test, expect, gotoConsole, navTo, navToRoute } from "./fixtures";
import { KIND, PERM } from "../src/app/lib/permissions-copy";

const PHONE = { width: 390, height: 844 };

async function boxOf(target: Locator): Promise<{ x: number; y: number; width: number; height: number }> {
  const box = await target.boundingBox();
  if (!box) throw new Error("element has no layout box");
  return box;
}

// Load the console at the default viewport (at 390px the sidebar link gotoConsole waits for is not rendered), then
// shrink to a phone and client-side navigate, as providers.spec.ts does.
async function phoneAt(page: Page, path: string): Promise<void> {
  await gotoConsole(page, "admin");
  await page.setViewportSize(PHONE);
  await navToRoute(page, path);
}

test.describe("Tier B at desktop width", () => {
  // #2852: the actor chip and the outcome differ in width from row to row; the description and the run chip do not move.
  test("Audit: the description and the run chip start at the same x on every row", async ({ page }) => {
    const row = (id: string, actor_type: string, outcome: string) => ({
      id: `e2e-align-${id}`,
      time: new Date().toISOString(),
      actor_type,
      actor: actor_type === "human" ? "alice@example.com" : "wardyn",
      action: "run.create",
      target: `align-${id}`,
      outcome,
      run_id: `run_e2ealign${id}`,
    });
    await page.route(/\/api\/v1\/audit(\?|$)/, (route) =>
      route.fulfill({ json: [row("a", "human", "success"), row("b", "system", "failure")] }),
    );
    await gotoConsole(page, "admin");
    await navTo(page, "Audit");
    const desc = page.locator('main [title^="Created the run"]');
    await expect(desc).toHaveCount(2);
    const chips = [page.getByRole("button", { name: "e2ealigna" }), page.getByRole("button", { name: "e2ealignb" })];
    await expect(chips[0]).toBeVisible();
    expect((await boxOf(desc.nth(0))).x).toBeCloseTo((await boxOf(desc.nth(1))).x, 0);
    expect((await boxOf(chips[0])).x).toBeCloseTo((await boxOf(chips[1])).x, 0);
    await page.unrouteAll({ behavior: "ignoreErrors" });
  });

  // #2899: the required marker no longer grows the Host label row, so its input sits level with its neighbours.
  test("Permissions: the required Host field sits level with the Who field beside it", async ({ page }) => {
    await gotoConsole(page, "admin");
    await navTo(page, "Permissions");
    const who = page.getByRole("textbox", { name: PERM.FIELD_WHO, exact: true });
    const host = page.getByRole("textbox", { name: KIND.egress_host.valueLabel, exact: true });
    await expect(host).toBeVisible();
    expect((await boxOf(host)).y).toBeCloseTo((await boxOf(who)).y, 0);
  });

  // #2892: Open and Delete… both lead with an icon, so their labels start at one x.
  test("Workspaces: the row menu's Open and Delete… start at the same x", async ({ page }) => {
    await gotoConsole(page, "admin");
    await navTo(page, "Workspaces");
    await page.getByRole("button", { name: "Workspace actions" }).first().click();
    const open = page.getByRole("menuitem", { name: "Open" });
    const del = page.getByRole("menuitem", { name: /^Delete/ });
    await expect(open).toBeVisible();
    await expect(open.locator("svg")).toHaveCount(1);
    expect((await boxOf(open.locator("svg"))).x).toBeCloseTo((await boxOf(del.locator("svg"))).x, 0);
  });
});

test.describe("Tier B at 390px", () => {
  // #2891: the count drops whole to one line instead of wrapping between the search box and Refresh.
  test("Workspaces: the list count stays on one line inside the viewport", async ({ page }) => {
    await phoneAt(page, "/admin/workspaces");
    const count = page.getByText(/^\d+ of \d+ workspaces?$/);
    await expect(count).toBeVisible();
    const box = await boxOf(count);
    expect(box.height).toBeLessThan(26);
    expect(box.x + box.width).toBeLessThanOrEqual(PHONE.width);
  });
});
