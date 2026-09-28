/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #1197 L4 — the Admin view's own sections (H-3), Everyone/Mine (H-4), saved
// views (H-8) and Group by (H-6). design.md §6's L4 row: "an owner's sign-in
// shows under Waiting on the owner with no button; Mine narrows; a saved view
// survives a reload; Group by Title works."
//
// Same technique as runs-landing.spec.ts: the list endpoint is fully replaced
// via page.route (real attention/scope combinations aren't worth a live seed
// script), so this file needs its own copy of that helper rather than
// importing runs-landing.spec.ts's private one.
import { test, expect, gotoConsole } from "./fixtures";
import type { Page } from "@playwright/test";
import { runsViewSaved } from "../src/app/components/wardyn/copy/runs-landing";

const RUNS_LIST_RE = /\/api\/v1\/runs(\?.*)?$/;

function baseRun(id: string, task: string, over: Record<string, unknown> = {}): Record<string, unknown> {
  const now = new Date().toISOString();
  return {
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
  };
}

type RunsListHandler = (url: URL) => { runs: unknown[] };

async function mockRunsList(page: Page, handler: RunsListHandler): Promise<void> {
  await page.route(RUNS_LIST_RE, async (route) => {
    const url = new URL(route.request().url());
    // core.ts's probeAuth fires a bare ?limit=1 read on every cold document
    // load — let it hit the real backend instead of this page's own scenario.
    if (!url.searchParams.has("view")) return route.fallback();
    const { runs } = handler(url);
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      headers: { "X-Wardyn-Hidden-Older": "0", "X-Wardyn-Hidden-Killed": "0" },
      body: JSON.stringify(runs),
    });
  });
}

test.describe("Runs Admin view — sections, Everyone/Mine, saved views, Group by (design.md §6 L4 row)", () => {
  test("an owner's sign-in shows under Waiting on the owner, with no button", async ({ page }) => {
    await mockRunsList(page, () => ({
      runs: [
        baseRun("o1", "Rotate the staging bucket policy", {
          state: "RUNNING",
          created_by: "someone-else",
          attention: { kind: "reauth", by: "owner", pending: 1 },
        }),
      ],
    }));
    await gotoConsole(page, "admin");

    const heading = page.getByRole("heading", { name: "Waiting on the owner" });
    await expect(heading).toBeVisible();
    const row = page.getByTestId("run-row").filter({ hasText: "Rotate the staging bucket policy" });
    await expect(row).toBeVisible();
    await expect(row.getByText("Waiting for the owner's AWS sign-in")).toBeVisible();
    await expect(row.getByRole("button")).toHaveCount(0);
    // Not "Needs a decision" — an admin cannot act on someone else's sign-in.
    await expect(page.getByRole("heading", { name: "Needs a decision" })).toHaveCount(0);
  });

  test("Mine narrows the list to the admin's own runs", async ({ page }) => {
    await mockRunsList(page, (url) => {
      const mineOnly = url.searchParams.get("owner") === "me";
      return {
        runs: mineOnly
          ? [baseRun("m1", "My own run", { created_by: "admin-token" })]
          : [
              baseRun("m1", "My own run", { created_by: "admin-token" }),
              baseRun("t1", "Someone else's run", { created_by: "someone-else" }),
            ],
      };
    });
    await gotoConsole(page, "admin");
    await expect(page.getByText("My own run")).toBeVisible();
    await expect(page.getByText("Someone else's run")).toBeVisible();

    await page.getByRole("combobox", { name: "Whose runs" }).click();
    await page.getByRole("option", { name: "Mine" }).click();

    await expect(page).toHaveURL(/owner=me/);
    await expect(page.getByText("My own run")).toBeVisible();
    await expect(page.getByText("Someone else's run")).toHaveCount(0);
  });

  test("the Saved view select never goes blank — it falls back to View · Custom once a filter changes", async ({
    page,
  }) => {
    await mockRunsList(page, () => ({ runs: [baseRun("r1", "A run")] }));
    await gotoConsole(page);
    const trigger = page.getByRole("combobox", { name: "Saved view" });
    await expect(trigger).toHaveText("View · Default");

    await page.getByLabel("Search runs", { exact: true }).fill("something");
    await expect(trigger).toHaveText("View · Custom");
  });

  test("picking a saved view in the Admin view keeps Mine, not reset to Everyone", async ({ page }) => {
    await mockRunsList(page, (url) => {
      const mineOnly = url.searchParams.get("owner") === "me";
      return {
        runs: mineOnly
          ? [baseRun("m1", "My own run", { created_by: "admin-token" })]
          : [
              baseRun("m1", "My own run", { created_by: "admin-token" }),
              baseRun("t1", "Someone else's run", { created_by: "someone-else" }),
            ],
      };
    });
    await gotoConsole(page, "admin");
    await page.getByRole("combobox", { name: "Whose runs" }).click();
    await page.getByRole("option", { name: "Mine" }).click();
    await expect(page).toHaveURL(/owner=me/);

    // The built-in "Default" view carries no owner param at all — Mine must
    // survive picking it anyway (H-4 is who's asking, not something a saved
    // view remembers).
    await page.getByRole("combobox", { name: "Saved view" }).click();
    await page.getByRole("option", { name: "View · Default" }).click();

    await expect(page).toHaveURL(/owner=me/);
    await expect(page.getByRole("combobox", { name: "Whose runs" })).toHaveText("Mine");
    await expect(page.getByText("Someone else's run")).toHaveCount(0);
  });

  test("a saved view survives a reload", async ({ page }) => {
    await mockRunsList(page, (url) => ({
      runs:
        url.searchParams.get("status") === "failed"
          ? [baseRun("f1", "A failed run", { state: "FAILED", ended_at: new Date().toISOString() })]
          : [baseRun("r1", "A running run", { state: "RUNNING" })],
    }));
    await gotoConsole(page);
    await expect(page.getByText("A running run")).toBeVisible();

    await page.getByRole("combobox", { name: "Status" }).click();
    await page.getByRole("option", { name: "Failed", exact: true }).click();
    await expect(page).toHaveURL(/status=failed/);
    await expect(page.getByText("A failed run")).toBeVisible();

    await page.getByRole("button", { name: "Save view" }).click();
    const nameInput = page.getByLabel("Name this view");
    await expect(nameInput).toHaveValue("My view");
    await nameInput.fill("My failures");
    await page.getByRole("button", { name: "Save", exact: true }).click();
    await expect(page.getByText(runsViewSaved("My failures")).first()).toBeVisible();

    // Back to plain /runs (defaults), then a REAL reload — the saved view
    // must come back from localStorage, not from anything still in memory.
    await page.goto("/runs");
    await expect(page.getByText("A running run")).toBeVisible();
    await page.reload();
    await expect(page.getByText("A running run")).toBeVisible();

    await page.getByRole("combobox", { name: "Saved view" }).click();
    await page.getByRole("option", { name: "View · My failures" }).click();
    await expect(page).toHaveURL(/status=failed/);
    await expect(page.getByText("A failed run")).toBeVisible();
  });

  test("Group by Title works", async ({ page }) => {
    await mockRunsList(page, () => ({
      runs: [
        baseRun("a1", "Weekly docs link check", {
          state: "COMPLETED",
          ended_at: new Date().toISOString(),
          repo: "acme/docs-site",
        }),
        baseRun("a2", "Weekly docs link check", {
          state: "COMPLETED",
          ended_at: new Date().toISOString(),
          repo: "acme/docs-site",
        }),
        baseRun("b1", "Something else entirely", {
          state: "COMPLETED",
          ended_at: new Date().toISOString(),
          repo: "acme/other",
        }),
      ],
    }));
    await gotoConsole(page);
    await expect(page.getByRole("heading", { name: "Ended today" })).toBeVisible();

    await page.getByRole("combobox", { name: "Group by" }).click();
    await page.getByRole("option", { name: "Group · Title" }).click();
    await expect(page).toHaveURL(/group=title/);

    // The two "Weekly docs link check" runs share one section titled after
    // the run — the time sections (Ended today / Earlier this week) are gone.
    const group = page.getByRole("heading", { name: "Weekly docs link check" });
    await expect(group).toBeVisible();
    await expect(page.getByTestId("run-row").filter({ hasText: "Weekly docs link check" })).toHaveCount(2);
    await expect(page.getByRole("heading", { name: "Something else entirely" })).toBeVisible();
    await expect(page.getByTestId("run-row").filter({ hasText: "Something else entirely" })).toHaveCount(1);
    await expect(page.getByRole("heading", { name: "Ended today" })).toHaveCount(0);
  });
});
