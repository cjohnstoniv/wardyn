/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Sandbox card of a live macOS VM run shows the guest's own numbers (the card says the guest reported
// them): CPU as a percent, memory as GB in use against the VM's allocation, a disk reading, a process count.
// "not available on this barrier" in any of the four fails. Skips unless the run is RUNNING.
import type { RunDetail } from "../../src/app/lib/types";
import { RUN_COCKPIT } from "../../src/app/components/wardyn/copy";
import { test, expect } from "./console";

test("the Sandbox card shows the guest's CPU, memory, disk and processes", async ({ site, page }) => {
  const run = await site.api<RunDetail>(`api/v1/runs/${site.runId}`);
  test.skip(run.state !== "RUNNING", `the run is ${run.state}, not RUNNING`);

  await page.goto(site.url(`runs/${site.runId}`));
  const card = page.locator("section").filter({ has: page.getByRole("heading", { level: 2, name: "Sandbox", exact: true }) });
  await expect(card).toBeVisible();
  const value = (label: string) => card.getByText(label, { exact: true }).locator("xpath=following-sibling::span");
  // The card polls every few seconds; the first read can still be the loading skeleton.
  await expect(value("CPU")).toHaveText(/^\d+%$/, { timeout: 30_000 });
  await expect(value("Memory")).toHaveText(/^\d+(\.\d)? GB \/ \d+(\.\d)? GB$/);
  await expect(value("Disk")).toHaveText(/^\d/); // a size, never "not available on this barrier"
  await expect(value("Processes")).toHaveText(/^\d+$/);
  await expect(card.getByText(RUN_COCKPIT.sandboxGuestReported)).toBeVisible();
});
