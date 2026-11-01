/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The watched desktop of a macOS VM run whose Screen Sharing is view only (macos.desktop "observe"): the
// viewer opens from the run page and its canvas holds a frame that is not blank. A frame is read back from the
// canvas itself, so a black or single-colour canvas fails. Skips, saying why, unless the run is RUNNING, is
// observe, and the UI gateway is on. Whether pointer input stays out of the guest is another row's job.
import type { RunDetail } from "../../src/app/lib/types";
import { DESKTOP } from "../../src/app/components/wardyn/copy/desktop";
import { test, expect } from "./console";

const FRAME_TIMEOUT = 90_000;
const MIN_COLOURS = 4;

test("the viewer shows a non-blank frame on an observe run", async ({ site, page, context }) => {
  const run = await site.api<RunDetail>(`api/v1/runs/${site.runId}`);
  test.skip(run.state !== "RUNNING", `the run is ${run.state}, not RUNNING`);
  test.skip(run.macos?.desktop !== "observe", "the run's desktop is not view only (macos.desktop is not observe)");
  const health = await site.api<{ ui_sandbox?: { enabled?: boolean } }>("healthz");
  test.skip(!health.ui_sandbox?.enabled, "the UI gateway is off, so there is no viewer to open");

  await page.goto(site.url(`runs/${site.runId}`));
  const desk = page.getByRole("region", { name: DESKTOP.title });
  await desk.scrollIntoViewIfNeeded();
  await expect(desk.getByText(DESKTOP.viewOnlyChip)).toBeVisible();
  const [viewer] = await Promise.all([context.waitForEvent("page"), desk.getByRole("button", { name: DESKTOP.watch }).click()]);

  const canvas = viewer.getByTestId("desktop-canvas");
  await expect(canvas).toBeVisible({ timeout: FRAME_TIMEOUT });
  await expect(viewer.getByText(DESKTOP.connected, { exact: true })).toBeVisible();
  await expect(viewer.getByText(DESKTOP.viewOnlyTitle, { exact: true })).toBeVisible();
  // Distinct colours among sampled pixels: a blank, black or single-colour canvas has one.
  await expect
    .poll(
      () =>
        canvas.evaluate((el, cap) => {
          const c = el as HTMLCanvasElement;
          if (!c.width || !c.height) return 0;
          const px = c.getContext("2d")?.getImageData(0, 0, c.width, c.height).data;
          if (!px) return 0;
          const seen = new Set<number>();
          for (let i = 0; i < px.length && seen.size < cap; i += 4 * 61) seen.add((px[i] << 16) | (px[i + 1] << 8) | px[i + 2]);
          return seen.size;
        }, MIN_COLOURS),
      { timeout: FRAME_TIMEOUT, message: "the canvas never held a frame with more than one colour" },
    )
    .toBeGreaterThanOrEqual(MIN_COLOURS);
});
