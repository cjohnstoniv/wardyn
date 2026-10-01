/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// F1 (#1197 L5, PR #1317 review) — briefs/1197/design.md:158's own pin: "a
// lost run is in Needs you, Revive moves it to Running; a lease-ended run is
// grey under Ended". Same GET /runs splice technique runs-landing.spec.ts
// uses (a precise attention/lost_reason combination no seed script produces).
import { test, expect, gotoConsole } from "./fixtures";
import type { Page } from "@playwright/test";

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

// Mutable across the test's own lifetime — a real Revive changes what the
// NEXT poll returns, which is exactly the "moves it to Running" half of
// design.md's own pin.
async function mockMutableRunsList(page: Page, initial: unknown[]) {
  let rows = initial;
  await page.route(RUNS_LIST_RE, async (route) => {
    const url = new URL(route.request().url());
    if (!url.searchParams.has("view")) return route.fallback();
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      headers: { "X-Wardyn-Hidden-Older": "0", "X-Wardyn-Hidden-Killed": "0" },
      body: JSON.stringify(rows),
    });
  });
  return { setRows: (next: unknown[]) => { rows = next; } };
}

test.describe("Runs landing — lost and lease-ended rows (F1, design.md:158)", () => {
  test("a lost run (by=you) is in Needs you, and Revive moves it to Running", async ({ page }) => {
    const lostId = "lost-run-1";
    const list = await mockMutableRunsList(page, [
      baseRun(lostId, "e2e lost run", { attention: { kind: "lost", by: "you", pending: 0 } }),
    ]);
    let revived = false;
    await page.route(`**/api/v1/runs/${lostId}/revive`, async (route) => {
      revived = true;
      list.setRows([baseRun(lostId, "e2e lost run")]); // the server's own post-revive shape: RUNNING, no attention
      await route.fulfill({ json: { run_id: lostId, denied_added: [], proxy_release: "r1" } });
    });

    await gotoConsole(page);
    await expect(page.getByRole("heading", { name: "Needs you" })).toBeVisible();
    const row = page.getByTestId("run-row").filter({ hasText: "e2e lost run" });
    await expect(row).toBeVisible();
    await expect(row.getByText("Sandbox stopped")).toBeVisible();

    const reviveButton = row.getByRole("button", { name: "Revive" });
    await expect(reviveButton).toBeVisible();
    await reviveButton.click();
    await expect.poll(() => revived).toBe(true);

    // "Revive moves it to Running": the next poll drops the row from Needs
    // you and the row itself now reads Running.
    await expect(page.getByRole("heading", { name: "Needs you" })).toHaveCount(0);
    await expect(page.getByTestId("run-row").filter({ hasText: "e2e lost run" }).getByText("Running")).toBeVisible();
  });

  test("a lease-ended run is grey under Ended, never in Needs you, with no Revive button", async ({ page }) => {
    const endedId = "ended-run-1";
    await mockMutableRunsList(page, [
      baseRun(endedId, "e2e lease-ended run", {
        lost_reason: "ended",
        // An hour ago, but never before today's midnight: a run that ended
        // yesterday sits under the collapsed "Earlier" group, not "Ended today".
        lost_at: new Date(Math.max(Date.now() - 3600_000, new Date().setHours(0, 0, 1, 0))).toISOString(),
      }),
    ]);

    await gotoConsole(page);
    await expect(page.getByRole("heading", { name: "Needs you" })).toHaveCount(0);
    const row = page.getByTestId("run-row").filter({ hasText: "e2e lease-ended run" });
    await expect(row).toBeVisible();
    await expect(row.getByText("Ended at its end time")).toBeVisible();
    await expect(row.getByRole("button", { name: "Revive" })).toHaveCount(0);
  });
});
