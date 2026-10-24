/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #2108 — Review's blocker for a secret the person has not stored carries the Add door Tools has. Saving stores
// the value (the real secrets API), Review asks the server again, the blocker goes once the server reports the
// secret stored, and only then does Launch run open. The preflight is spliced to say so; the secret store is real.
import { test, expect, gotoConsole, openNewRun, walkGuidedToReview } from "./fixtures";
import { NR_REVIEW } from "../src/app/components/wardyn/copy/new-run/review";
import { NR_TOOLS } from "../src/app/components/wardyn/copy/new-run/tools";

const NAME = "e2e_add_door_token";

test.describe("Review: Add a secret that is not stored (#2108)", () => {
  test.afterEach(async ({ request }) => {
    await request.delete(`/api/v1/secrets/${NAME}`, { headers: { Authorization: `Bearer ${process.env.WARDYN_E2E_TOKEN ?? ""}` } });
  });

  test("Add at the blocker stores the secret, the blocker goes, and Launch run opens", async ({ page }) => {
    let stored = false;
    await page.route("**/api/v1/runs/preflight", (route) =>
      route.fulfill({
        json: {
          enforced_confinement_class: "CC1",
          setup_items: stored
            ? []
            : [{ id: `secret:${NAME}`, kind: "secret", label: `Secret: ${NAME}`, required_by: "Application", status: "missing", fix: { action: "add_secret", secret_name: NAME } }],
        },
      }),
    );
    await page.route(`**/api/v1/secrets/${NAME}`, async (route) => {
      const response = await route.fetch();
      if (route.request().method() === "PUT" && response.ok()) stored = true;
      await route.fulfill({ response });
    });

    await gotoConsole(page);
    await openNewRun(page);
    await walkGuidedToReview(page);

    const launch = page.getByRole("button", { name: NR_REVIEW.LAUNCH_RUN });
    await expect(launch).toBeDisabled();
    const blockers = page.locator("#nr-blockers");
    await expect(blockers.getByRole("button", { name: `Secret: ${NAME}` })).toBeVisible();

    // exact: the blocker's own label button, "Secret: e2e_add_door_token", has "add" in its name too.
    await blockers.getByRole("button", { name: NR_TOOLS.SETUP_ADD, exact: true }).click();
    const dialog = page.getByRole("dialog", { name: `Secret: ${NAME}` });
    await expect(dialog.getByLabel(NR_TOOLS.TOKEN_LABEL)).toHaveValue("");
    await expect(dialog.getByRole("button", { name: NR_TOOLS.TOKEN_CONFIRM })).toBeDisabled();
    await dialog.getByLabel(NR_TOOLS.TOKEN_LABEL).fill("a-token-value");
    await dialog.getByRole("button", { name: NR_TOOLS.TOKEN_CONFIRM }).click();

    await expect(dialog).toHaveCount(0);
    await expect(blockers.getByRole("button", { name: `Secret: ${NAME}` })).toHaveCount(0);
    await expect(launch).toBeEnabled();
  });

  test("a disabled Launch run does not read as enabled: aria-disabled and the grey fill", async ({ page }) => {
    await page.route("**/api/v1/runs/preflight", (route) =>
      route.fulfill({
        json: {
          enforced_confinement_class: "CC1",
          setup_items: [{ id: `secret:${NAME}`, kind: "secret", label: `Secret: ${NAME}`, required_by: "Application", status: "missing", fix: { action: "add_secret", secret_name: NAME } }],
        },
      }),
    );
    await gotoConsole(page);
    await openNewRun(page);
    await walkGuidedToReview(page);

    const launch = page.getByRole("button", { name: NR_REVIEW.LAUNCH_RUN });
    await expect(launch).toBeDisabled();
    await expect(launch).toHaveAttribute("aria-disabled", "true");
    await expect(launch).toHaveCSS("opacity", "0.5");
    // Dark is the console's default: the fill is the muted token, not the primary teal.
    const [fill, muted, primary] = await launch.evaluate((el) => {
      const probe = (cls: string) => {
        const d = document.createElement("div");
        d.className = cls;
        document.body.appendChild(d);
        const c = getComputedStyle(d).backgroundColor;
        d.remove();
        return c;
      };
      return [getComputedStyle(el).backgroundColor, probe("bg-muted"), probe("bg-primary")];
    });
    expect(fill).toBe(muted);
    expect(fill).not.toBe(primary);
  });
});
