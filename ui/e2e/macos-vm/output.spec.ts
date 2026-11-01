/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Output tab of a macOS VM run returns the guest's output through the API: a source label and a pane of
// text, not an empty state. When the check script prints a marker and names it in WARDYN_MACVM_CONSOLE_OUTPUT_MARK,
// that text is in the pane. Skips, saying why, for a run that is still starting.
import type { RunDetail } from "../../src/app/lib/types";
import { RUN_OUTPUT } from "../../src/app/components/wardyn/copy";
import { test, expect, marker } from "./console";

test("the Output tab shows the run's output", async ({ site, page }) => {
  const run = await site.api<RunDetail>(`api/v1/runs/${site.runId}`);
  test.skip((["PENDING", "QUEUED", "STARTING"] as string[]).includes(run.state), `the run is ${run.state}, so it has no output yet`);

  await page.goto(site.url(`runs/${site.runId}`));
  await page.getByRole("tab", { name: RUN_OUTPUT.tab }).click();
  const pane = page.getByTestId("run-output-text");
  await expect(pane).toBeVisible({ timeout: 60_000 });
  await expect(page.getByText(RUN_OUTPUT.sourceStdout).or(page.getByText(RUN_OUTPUT.sourcePane))).toBeVisible();
  const planted = marker("WARDYN_MACVM_CONSOLE_OUTPUT_MARK");
  if (planted) await expect(pane).toContainText(planted, { timeout: 60_000 });
  else {
    await expect(pane).not.toContainText(RUN_OUTPUT.emptyLive);
    await expect(pane).not.toContainText(RUN_OUTPUT.emptyFinal);
  }
});
