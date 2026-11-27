/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Launched screen follows the run's event stream while the run starts: it reads the run the moment the stream
// says it changed, not at the next 4-second refresh.
import { test, expect, gotoConsole, navToRoute } from "./fixtures";
import { NR_LAUNCH } from "../src/app/components/wardyn/copy/new-run/launch";

const RUN_ID = "c3333333-3333-4333-8333-333333333333";

test("the Launched screen shows the run running as soon as its stream says so", async ({ page }) => {
  let ready = false;
  await page.route(`**/api/v1/runs/${RUN_ID}/events`, async (route) => {
    await new Promise((resolve) => setTimeout(resolve, 4500));
    ready = true;
    await route
      .fulfill({ status: 200, contentType: "text/event-stream", body: "id: 1\nevent: ready\ndata: {}\n\n" })
      .catch(() => {});
  });
  await page.route(`**/api/v1/runs/${RUN_ID}`, (route) =>
    route.fulfill({ json: { id: RUN_ID, state: ready ? "RUNNING" : "STARTING" } }),
  );

  await gotoConsole(page);
  const events = page.waitForResponse(`**/api/v1/runs/${RUN_ID}/events`);
  await navToRoute(page, `/runs/new?launched=${RUN_ID}`);
  await expect(page.getByText(NR_LAUNCH.BADGE_STARTING, { exact: true })).toBeVisible();
  await events;
  // The polls fall at about 4 s (before the event, so Starting) and about 8 s; only the event's read passes in 2.5 s.
  await expect(page.getByText(NR_LAUNCH.BADGE_STARTING, { exact: true })).toBeHidden({ timeout: 2500 });
});
