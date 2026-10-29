/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Workloads screen: a workload applied from the command line is listed with its source, and its drawer
// opens on its spec. The list is stubbed at its route: the seeded backend holds no workloads, and applying one
// needs the CLI.
import { test, expect, gotoConsole, navTo } from "./fixtures";
import { aheadByHours } from "../src/app/lib/test-clock";
import type { Workload } from "../src/app/lib/types/workloads";

const CLI_APPLIED: Workload = {
  name: "triage-bot",
  owner: "admin",
  source: "api",
  intent: { shape: "automation", experience: "background" },
  generation: 1,
  current_generation: 0,
  created_at: aheadByHours(-1),
  updated_at: aheadByHours(-1),
};

test("list shows a CLI-applied workload", async ({ page }) => {
  await page.route("**/api/v1/workloads?*", (route) => route.fulfill({ json: [CLI_APPLIED] }));
  await gotoConsole(page);
  await navTo(page, "Workloads");
  await expect(page.getByRole("heading", { name: "Workloads", level: 1 })).toBeVisible();
  const row = page.getByTestId("workload-row").filter({ hasText: "triage-bot" });
  await expect(row).toContainText("Wardyn CLI or API");
  await expect(row).toContainText("Queued");
  await row.getByRole("button", { name: "triage-bot" }).click();
  const drawer = page.getByRole("dialog");
  await expect(drawer.getByText("Spec")).toBeVisible();
  await expect(drawer.getByRole("button", { name: "Restart" })).toBeVisible();
  await expect(drawer.getByRole("button", { name: "Delete" })).toBeVisible();
});
