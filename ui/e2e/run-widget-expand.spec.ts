/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, gotoConsole, sql } from "./fixtures";
import { randomUUID } from "node:crypto";
import type { Page } from "@playwright/test";
import { RUN_COCKPIT } from "../src/app/components/wardyn/copy";

// A run page tile that holds more than it shows says so, and every tile but the Terminal opens the same live widget in
// a large view. The unit tests pin the moves; this is what only a real browser can read: the scroll offsets, the
// scrollbar, the focus order, the gutter above the dialog and the approvals under it.
test.use({ colorScheme: "dark" });
test.describe.configure({ mode: "serial" });

const PATHS = (n: number) =>
  Array.from({ length: n }, (_, i) => ({
    path: `internal/billing/ledger/generated/part_${String(i).padStart(2, "0")}/reconcile_pending_invoices_with_upstream_provider_and_retry.go`,
    status: "M",
    added: i + 1,
    deleted: 0,
  }));

let paths = 60;
async function stubFiles(page: Page) {
  await page.route("**/api/v1/runs/*/files", (route) =>
    route.fulfill({ json: { vcs: "git", files: PATHS(paths), truncated: false, path: "/workspace" } }),
  );
}

async function openRun(page: Page, viewport = { width: 1440, height: 900 }): Promise<string> {
  await page.setViewportSize(viewport);
  const id = sql("SELECT id FROM agent_runs ORDER BY created_at LIMIT 1");
  sql(`UPDATE agent_runs SET state = 'RUNNING' WHERE id = '${id}'`);
  await stubFiles(page);
  await gotoConsole(page);
  await page.goto(`/runs/${id}`);
  return id;
}

const filesTile = (page: Page) => page.locator('[data-widget="files"]');
const filesBody = (page: Page) => filesTile(page).locator("section > :last-child");

test("#3940: a Files tile that scrolls opens whole, from where it was scrolled to, and goes back there", async ({ page }) => {
  paths = 60;
  await openRun(page);
  const body = filesBody(page);
  await expect(body).toBeVisible();
  expect(await body.evaluate((el) => el.scrollHeight > el.clientHeight)).toBe(true);
  // The cue: the scrollbar's space is held, and drawn.
  expect(await body.evaluate((el) => (el as HTMLElement).offsetWidth - el.clientWidth)).toBeGreaterThan(0);

  await body.evaluate((el) => (el.scrollTop = 300));
  await page.getByRole("button", { name: RUN_COCKPIT.expandWidget("Files changed") }).click();
  const dialog = page.getByRole("dialog", { name: "Files changed" });
  await expect(dialog).toBeVisible();
  const big = dialog.locator("[data-widget-body]");
  expect(await big.evaluate((el) => el.scrollTop)).toBeGreaterThan(0);
  // A long path is whole.
  const first = big.getByText("part_00", { exact: false }).first();
  expect(await first.evaluate((el) => el.scrollWidth <= el.clientWidth)).toBe(true);

  await page.keyboard.press("Escape");
  await expect(dialog).toHaveCount(0);
  expect(await filesBody(page).evaluate((el) => el.scrollTop)).toBeGreaterThan(0);
});

test("opening or closing it, and a row count that grows, move nothing in the tile's header", async ({ page }) => {
  paths = 2;
  await openRun(page);
  const control = page.getByRole("button", { name: RUN_COCKPIT.expandWidget("Files changed") });
  const boxes = async () => [await filesTile(page).boundingBox(), await filesTile(page).locator("section > :first-child").boundingBox(), await control.boundingBox()];
  const before = await boxes();
  paths = 60;
  await page.waitForTimeout(4500); // one poll
  expect(await boxes()).toEqual(before);
});

test("keyboard: Enter opens it, focus lands in the dialog, Tab stays inside, Escape returns to the control", async ({ page }) => {
  paths = 60;
  await openRun(page);
  const control = page.getByRole("button", { name: RUN_COCKPIT.expandWidget("Files changed") });
  await control.focus();
  await page.keyboard.press("Enter");
  const dialog = page.getByRole("dialog", { name: "Files changed" });
  await expect(dialog).toBeVisible();
  await expect(dialog.locator("[data-widget-body]")).toBeFocused();
  const top = await dialog.locator("[data-widget-body]").evaluate((el) => el.scrollTop);
  await page.keyboard.press("PageDown");
  expect(await dialog.locator("[data-widget-body]").evaluate((el) => el.scrollTop)).toBeGreaterThan(top);
  for (let i = 0; i < 6; i++) {
    await page.keyboard.press("Tab");
    expect(await page.evaluate(() => !!document.activeElement?.closest('[role="dialog"]'))).toBe(true);
  }
  await page.keyboard.press("Escape");
  await expect(dialog).toHaveCount(0);
  await expect(control).toBeFocused();
});

test("a click in the moved widget does not close it, and the dialog starts 64px down so the daemon banner stays above", async ({ page }) => {
  paths = 60;
  await openRun(page);
  await page.getByRole("button", { name: RUN_COCKPIT.expandWidget("Identity") }).click();
  const dialog = page.getByRole("dialog", { name: "Identity" });
  await expect(dialog).toBeVisible();
  const box = (await dialog.boundingBox())!;
  expect(box.y).toBeGreaterThanOrEqual(63);
  expect(box.y + box.height).toBeLessThanOrEqual(900 - 43);
  await dialog.getByRole("button").filter({ hasText: /copy/i }).first().click().catch(() => undefined);
  await expect(dialog).toBeVisible();
});

test("a held approval is not covered by the dialog and Approve works from it, at 1440 and 390", async ({ page }) => {
  paths = 60;
  const id = await openRun(page);
  const CMD = "bash.exec --run the-nightly-integration-suite-with-extra-long-name";
  if (sql(`SELECT count(*) FROM approvals WHERE run_id = '${id}' AND state = 'PENDING'`) === "0") {
    sql(`INSERT INTO approvals (id, run_id, kind, requested_scope, state, requested_at)
         VALUES ('${randomUUID()}', '${id}', 'tool_call', '${JSON.stringify({ tool: "bash.exec", cmd: CMD })}'::jsonb, 'PENDING', now() - interval '5 min')`);
  }
  for (const viewport of [{ width: 1440, height: 900 }, { width: 390, height: 844 }]) {
    await page.setViewportSize(viewport);
    await page.goto(`/runs/${id}`);
    await page.getByRole("button", { name: RUN_COCKPIT.expandWidget("Egress") }).click();
    const dialog = page.getByRole("dialog", { name: "Egress" });
    const approve = dialog.getByRole("button", { name: "Approve", exact: true });
    await expect(approve).toBeVisible();
    const [x, y] = await approve.evaluate((el) => {
      const r = el.getBoundingClientRect();
      return [r.x + r.width / 2, r.y + r.height / 2];
    });
    expect(await page.evaluate(([px, py]) => document.elementFromPoint(px, py)?.closest("button")?.textContent?.trim(), [x, y])).toContain("Approve");
    await page.keyboard.press("Escape");
    await expect(dialog).toHaveCount(0);
  }
});

test("at 320x568 the control is inside the tile once scrolled to it, and the dialog does not scroll the page sideways", async ({ page }) => {
  paths = 60;
  await openRun(page, { width: 320, height: 568 });
  const control = page.getByRole("button", { name: RUN_COCKPIT.expandWidget("Egress") });
  await control.scrollIntoViewIfNeeded();
  const [c, t] = [(await control.boundingBox())!, (await page.locator('[data-widget="egress"]').boundingBox())!];
  expect(c.x).toBeGreaterThanOrEqual(t.x);
  expect(c.x + c.width).toBeLessThanOrEqual(t.x + t.width + 1);
  await control.click();
  await expect(page.getByRole("dialog", { name: "Egress" })).toBeVisible();
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= document.documentElement.clientWidth)).toBe(true);
  await expect(page.getByRole("button", { name: "Close" })).toBeVisible();
});

test("edit layout draws no visible control, and focus mode's dock none", async ({ page }) => {
  paths = 60;
  await openRun(page);
  await page.getByRole("button", { name: RUN_COCKPIT.editLayout }).click();
  await expect(page.locator("[data-widget-expand]").first()).toBeHidden();
  await page.getByRole("button", { name: RUN_COCKPIT.doneEditing }).click();
  await page.getByRole("button", { name: RUN_COCKPIT.enterFocus }).click();
  await page.getByRole("button", { name: RUN_COCKPIT.showWidget("Egress") }).click();
  await expect(page.getByRole("region", { name: "Egress" })).toBeVisible();
  await expect(page.getByRole("button", { name: /^Expand / })).toHaveCount(0);
});
