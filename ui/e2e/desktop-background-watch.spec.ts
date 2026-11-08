/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { Page } from "@playwright/test";
import { test, expect, sql, gotoConsole } from "./fixtures";
import { DESKTOP } from "../src/app/components/wardyn/copy/desktop";
import { RUN_MODE_REFUSAL } from "../src/app/lib/run-mode-refusals";

// ============================================================================
// The Desktop tile of a background run, and the Attach tile that is not drawn for one. The `none`
// runner behind this backend has no VM, so the seeded RUNNING run is shown as a background VM run on the wire; the
// door itself (the ticket, the gateway's view-only stream, the re-check) is proven by the Go gateway tests.
// ============================================================================

const RUN_TASK = "e2e fixture 2";

function runIdByTask(task: string): string {
  const id = sql(`SELECT id FROM agent_runs WHERE task = '${task}' LIMIT 1`);
  expect(id, `no seeded run with task ${task}`).toMatch(/^[0-9a-f-]{36}$/);
  return id;
}

/** The seeded run's page, shown as a background VM run with what over() adds. */
async function openBackgroundRun(page: Page, over: Record<string, unknown>): Promise<string> {
  const id = runIdByTask(RUN_TASK);
  await page.route(`**/api/v1/runs/${id}`, async (route) => {
    const res = await route.fetch();
    await route.fulfill({ response: res, body: JSON.stringify({ ...(await res.json()), experience: "background", ...over }) });
  });
  await page.route(`**/api/v1/runs/${id}/desktop/sign-ins`, (route) => route.fulfill({ status: 500, json: {} }));
  await gotoConsole(page);
  await page.goto(`/runs/${id}`);
  await expect(page.getByRole("heading", { name: RUN_TASK, level: 1 })).toBeVisible();
  return id;
}

const desktop = (page: Page) => page.getByRole("region", { name: DESKTOP.title });

test.describe("Run detail: a background run's Desktop tile", () => {
  test("a macOS run whose watch is open has Watch desktop first, the exposure words, and no Attach tile", async ({ page }) => {
    const id = await openBackgroundRun(page, { machine_kind: "macos_vm", background_watch: { state: "open" } });
    const desk = desktop(page);
    await desk.scrollIntoViewIfNeeded();
    await expect(desk.getByRole("button")).toHaveCount(1);
    await expect(desk.getByRole("button", { name: DESKTOP.watch })).toBeVisible();
    await expect(desk.getByText(DESKTOP.backgroundWatchTile)).toBeVisible();
    await expect(desk.getByText(DESKTOP.backgroundWatchExposure)).toBeVisible();
    await expect(desk.locator("xpath=ancestor::section[1]").getByText(DESKTOP.viewOnlyChip)).toBeVisible();
    await expect(page.getByText("Attach from your terminal")).toHaveCount(0);

    // Watch asks for the watch ticket, never the terminal's.
    let asked = "";
    await page.route(`**/api/v1/runs/${id}/desktop/watch-ticket`, async (route) => {
      asked = route.request().method();
      await route.fulfill({ json: { ticket: "tkt_e2e", expires_in_seconds: 60 } });
    });
    await desk.getByRole("button", { name: DESKTOP.watch }).click();
    await expect.poll(() => asked).toBe("POST");
  });

  test("a refused watch is one sentence from the server and no button", async ({ page }) => {
    const message = "Your organisation's governance profile doesn't allow watching a background run's desktop.";
    await openBackgroundRun(page, { machine_kind: "macos_vm", background_watch: { state: "governance", message } });
    const desk = desktop(page);
    await desk.scrollIntoViewIfNeeded();
    await expect(desk.getByText(message)).toBeVisible();
    await expect(desk.getByRole("button")).toHaveCount(0);
    await expect(page.getByText("Attach from your terminal")).toHaveCount(0);
  });

  test("a Linux VM background run keeps its existing no-desktop sentence", async ({ page }) => {
    await openBackgroundRun(page, { machine_kind: "linux_vm" });
    const desk = desktop(page);
    await desk.scrollIntoViewIfNeeded();
    await expect(desk.getByText(RUN_MODE_REFUSAL.BACKGROUND_ONLY("desktop"))).toBeVisible();
    await expect(desk.getByRole("button")).toHaveCount(0);
  });
});
