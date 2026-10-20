/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, ADMIN_TOKEN } from "./fixtures";
import { DESKTOP_SIGNIN_REASON_LINE } from "../src/app/lib/desktop-signin-reasons";

// A sign-in Wardyn could not close out kills its run with the reason
// desktop_signin_resume_failed. The run page's failure block says why in a sentence;
// the code never reaches the page. The kill row is spliced: the hermetic backend has
// no desktop sign-in to fail.
test.describe("Run cockpit — a run killed for a desktop sign-in", () => {
  test("says why in words and shows no reason code", async ({ page }) => {
    const auth = { Authorization: `Bearer ${ADMIN_TOKEN}` };
    const list = (await (await page.request.get("api/v1/runs?limit=1000", { headers: auth })).json()) as { id: string; task: string }[];
    const id = list.find((r) => r.task === "e2e fixture 7")!.id;
    const at = new Date().toISOString();
    await page.route(/\/api\/v1\/audit\?/, async (route) => {
      if (new URL(route.request().url()).searchParams.get("action") === "run.kill") {
        return route.fulfill({ json: [{ id: "k1", time: at, actor_type: "system", actor: "wardynd", action: "run.kill", outcome: "success", data: { reason: "desktop_signin_resume_failed", signin_id: "s1" } }] });
      }
      return route.fallback();
    });
    await page.goto(`runs/${id}`);
    const block = page.getByTestId("run-failure-block");
    await expect(block).toContainText(DESKTOP_SIGNIN_REASON_LINE.desktop_signin_resume_failed);
    await expect(block).not.toContainText("desktop_signin_resume_failed");
  });
});
