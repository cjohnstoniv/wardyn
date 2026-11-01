/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Files changed card of a live macOS VM run reads the guest's work folder through the API: it lists
// files, or says there are none yet, and never the card's refusal or error lines. When the check script plants
// a file and names it in WARDYN_MACVM_CONSOLE_FILE_NAME, that file is in the list and the Files tile counts it.
// Skips unless the run is RUNNING.
import type { RunDetail } from "../../src/app/lib/types";
import { RUN_COCKPIT } from "../../src/app/components/wardyn/copy";
import { test, expect, marker } from "./console";

test("the Files card reads the guest's work folder", async ({ site, page }) => {
  const run = await site.api<RunDetail>(`api/v1/runs/${site.runId}`);
  test.skip(run.state !== "RUNNING", `the run is ${run.state}, not RUNNING`);

  await page.goto(site.url(`runs/${site.runId}`));
  const card = page.locator("section").filter({ has: page.getByRole("heading", { level: 2, name: "Files changed", exact: true }) });
  await expect(card).toBeVisible();
  const planted = marker("WARDYN_MACVM_CONSOLE_FILE_NAME");
  if (planted) {
    await expect(card.getByText(planted)).toBeVisible({ timeout: 60_000 });
    const tile = page.getByTestId("status-tile").filter({ hasText: RUN_COCKPIT.tileFiles });
    await expect(tile).not.toContainText(RUN_COCKPIT.tileNoneYet);
    await expect(tile).not.toContainText(RUN_COCKPIT.tileNotInspectable);
    return;
  }
  // No file named: a real reading is a list or "No files changed yet.", not a refusal or an error line.
  await expect(card.getByText(RUN_COCKPIT.noFilesChanged).or(card.locator("div.divide-y"))).toBeVisible({ timeout: 60_000 });
});
