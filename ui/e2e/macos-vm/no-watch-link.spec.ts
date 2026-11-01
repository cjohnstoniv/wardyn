/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A macOS VM run is not recorded, and the Overview must not offer "Watch the captured session" for a run that
// has no recording: the link led to an empty Recording tab. The absence is read only after the page has drawn
// the terminal pane that would carry the link. Skips, saying why, if the run does have a recording.
import type { RunDetail } from "../../src/app/lib/types";
import { test, expect } from "./console";

test('no "Watch the captured session" link on an unrecorded run', async ({ site, page }) => {
  const run = await site.api<RunDetail>(`api/v1/runs/${site.runId}`);
  test.skip(run.has_recording !== false, "the run has a recording (has_recording is not false), so the link is allowed");

  await page.goto(site.url(`runs/${site.runId}`));
  await expect(page.getByRole("heading", { level: 1 })).toBeVisible();
  await expect(page.getByTestId("run-terminal-pane")).toBeVisible();
  // Let the recording and health reads that decide the link settle before reading its absence.
  await page.waitForLoadState("networkidle");
  await expect(page.getByText(/Watch the captured session/)).toHaveCount(0);
});
