/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #3041 — the Runs filter bar at phone width. At 390 only the search box stays
// on the row: a Filters (n) button opens a bottom sheet with every other
// control and Save view, and the active filters show as removable chips under
// the search, so the first run is on the first screen. From the tablet
// breakpoint up the bar is the full inline one it always was.
//
// The runs list is spliced (page.route), the technique runs-landing.spec.ts
// uses, so the first screen holds a known run.
import { test, expect, gotoConsole } from "./fixtures";
import type { Page } from "@playwright/test";

const RUNS_LIST_RE = /\/api\/v1\/runs(\?.*)?$/;
const PHONE = { width: 390, height: 844 };

async function mockRuns(page: Page): Promise<void> {
  const now = new Date().toISOString();
  const run = (id: string, task: string, over: Record<string, unknown> = {}) => ({
    id,
    task,
    title: task,
    created_at: now,
    updated_at: now,
    created_by: "admin-token",
    agent: "claude-code",
    repo: "acme/widgets",
    confinement_class: "CC2",
    state: "RUNNING",
    spiffe_id: `spiffe://${id}`,
    runner_target: "docker",
    ...over,
  });
  const runs = [
    run("a1", "payments deps bump", { attention: { kind: "approval", by: "you", pending: 1 } }),
    run("r1", "Rotate deploy keys"),
    run("r2", "Backfill ledger"),
  ];
  await page.route(RUNS_LIST_RE, async (route) => {
    // The shell's auth probe is the bare ?limit=1 read; only the page's own (?view=) request is spliced.
    if (!new URL(route.request().url()).searchParams.has("view")) return route.fallback();
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      headers: { "X-Wardyn-Hidden-Older": "0", "X-Wardyn-Hidden-Killed": "0" },
      body: JSON.stringify(runs),
    });
  });
}

// gotoConsole waits for the full-width sidebar, so land first and resize after.
async function landOn(page: Page, size: { width: number; height: number }): Promise<void> {
  await mockRuns(page);
  await gotoConsole(page);
  await page.setViewportSize(size);
  await expect(page.getByRole("heading", { name: "Runs", level: 1 })).toBeVisible();
}

test.describe("Runs filter bar at 390px (#3041)", () => {
  test("search, a Filters button and the first run on the first screen", async ({ page }) => {
    await landOn(page, PHONE);
    await expect(page.getByLabel("Search runs", { exact: true })).toBeVisible();
    await expect(page.getByRole("button", { name: "Filters", exact: true })).toBeVisible();
    // Every other control moved into the sheet (scoped to the bar: the composer above has its own Workspace select).
    for (const name of ["Status", "Ended within", "Workspace", "Group by", "Saved view"]) {
      await expect(page.getByRole("search").getByRole("combobox", { name })).toBeHidden();
    }
    await expect(page.getByRole("search").getByRole("button", { name: "Save view" })).toBeHidden();
    // The first run row is on the first screen, with no scrolling.
    await expect(page.getByRole("link", { name: /payments deps bump/ })).toBeInViewport({ ratio: 1 });
    expect(await page.evaluate(() => window.scrollY)).toBe(0);
  });

  test("the sheet holds the controls; a chip appears for a filter and removing it clears the filter", async ({ page }) => {
    await landOn(page, PHONE);
    await page.getByRole("button", { name: "Filters", exact: true }).click();
    const sheet = page.getByRole("dialog", { name: "Filters" });
    await expect(sheet).toBeVisible();
    await expect(sheet.getByRole("button", { name: "Save view" })).toBeVisible();
    await expect(sheet.getByRole("button", { name: "Show 3 runs" })).toBeVisible();

    await sheet.getByRole("combobox", { name: "Status" }).click();
    await page.getByRole("option", { name: "Status · Needs you" }).click();
    await expect(page).toHaveURL(/status=needs/);
    await sheet.getByRole("button", { name: "Show 3 runs" }).click();
    await expect(sheet).toBeHidden();

    await expect(page.getByRole("button", { name: "Filters (1)" })).toBeVisible();
    const chips = page.getByTestId("runs-filter-chips");
    await expect(chips).toContainText("Status · Needs you");
    await chips.getByRole("button", { name: "Remove Status · Needs you" }).click();
    await expect(page).not.toHaveURL(/status=/);
    await expect(chips).toHaveCount(0);
    await expect(page.getByRole("button", { name: "Filters", exact: true })).toBeVisible();
  });
});

test.describe("An open Filters sheet when the viewport grows (#3041)", () => {
  test("closes at 640px and up, leaving the page clickable", async ({ page }) => {
    await landOn(page, PHONE);
    await page.getByRole("button", { name: "Filters", exact: true }).click();
    await expect(page.getByRole("dialog", { name: "Filters" })).toBeVisible();
    await page.setViewportSize({ width: 1024, height: 768 });
    await expect(page.getByRole("dialog")).toHaveCount(0);
    // The Filters button is hidden from 640px, so focus goes to the search box, not the body.
    await expect(page.getByLabel("Search runs", { exact: true })).toBeFocused();
    // No leftover overlay or modal lock: the inline Status select takes a click.
    await page.getByRole("search").getByRole("combobox", { name: "Status" }).click();
    await expect(page.getByRole("option", { name: "Status · Needs you" })).toBeVisible();
  });
});

test.describe("Runs filter bar from the tablet breakpoint up (#3041)", () => {
  for (const size of [
    { width: 1440, height: 900 },
    { width: 1024, height: 768 },
  ]) {
    test(`${size.width}px: the full inline bar, no Filters button`, async ({ page }) => {
      await landOn(page, size);
      await page.goto("/runs?status=needs&group=workspace");
      await expect(page.getByLabel("Search runs", { exact: true })).toBeVisible();
      for (const name of ["Status", "Ended within", "Workspace", "Group by", "Saved view"]) {
        await expect(page.getByRole("search").getByRole("combobox", { name })).toBeVisible();
      }
      await expect(page.getByRole("search").getByLabel("Include killed")).toBeVisible();
      await expect(page.getByRole("search").getByRole("button", { name: "Save view" })).toBeVisible();
      await expect(page.getByRole("button", { name: /^Filters/ })).toBeHidden();
      await expect(page.getByTestId("runs-filter-chips")).toBeHidden();
    });
  }
});
