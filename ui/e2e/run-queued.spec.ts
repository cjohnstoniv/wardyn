/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, gotoConsole, navTo } from "./fixtures";
import { RUN_WAIT, RUN_WAIT_REASON } from "../src/app/lib/runner-pool-refusals";

// A QUEUED run's page: the header glyph reads Queued (not Stopped), and the hero says why the run waits and where
// it stands in line instead of being an empty frame. The seeded backend has no capped pool, so fixture 0's read is
// spliced into the QUEUED shape the server would answer (route.fetch + patch + refulfill, as run-startup-progress.spec.ts).
test.describe.configure({ mode: "serial" });

test.describe("Run page — a QUEUED run", () => {
  test("the header glyph reads Queued and the hero names the wait and the place in line", async ({ page }) => {
    await page.route("**/api/v1/runs/*", async (route) => {
      if (route.request().method() !== "GET") return route.fallback();
      const response = await route.fetch();
      const json = await response.json();
      if (json.task === "e2e fixture 0") {
        Object.assign(json, {
          state: "QUEUED",
          interactive: true,
          queue: {
            pool_id: "pool-e2e",
            queued_at: new Date(Date.now() - 5 * 60_000).toISOString(),
            deadline_at: new Date(Date.now() + 3 * 3600_000).toISOString(),
            position: 1,
            reason: RUN_WAIT_REASON.RECORDING_STORE_OFF,
          },
        });
      }
      await route.fulfill({ response, json });
    });

    await gotoConsole(page);
    await navTo(page, "Runs");
    await page.getByText("e2e fixture 0").click();
    await expect(page).toHaveURL(/\/runs\/[0-9a-f-]{8,}/);

    await expect(page.getByRole("img", { name: "Queued" })).toBeVisible();
    await expect(page.getByRole("img", { name: "Stopped" })).toHaveCount(0);
    const hero = page.getByTestId("run-queued-wait");
    await expect(hero.getByText(RUN_WAIT.RECORDING_STORE_OFF())).toBeVisible();
    await expect(hero.getByRole("heading", { name: "Queued" })).toBeVisible();
    await expect(hero.getByTestId("run-queued-wait-line")).toHaveText(
      /^You are 2nd in line · waiting since .+ · gives up in 3 hours$/,
    );
  });
});
