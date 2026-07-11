/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, gotoConsole, navTo } from "./fixtures";

// Getting Started funnel — hermetic walk against the seeded backend (real
// wardynd + Postgres + `none` runner, admin-token auth). The unit suites cover
// the branchy per-step logic; this spec proves the real wiring: sidebar entry →
// onboarding tour → SetupScreen, the full 9-step Next walk in STEP_ORDER
// (essentials → your work → corporate network → finish), honest not-ready
// gating on a runner-less host, and the Finish-later dismissal.
//
// Note the seeded backend's shape is load-bearing here: driver "none" means no
// barrier is ready and no model is connected, so the launch gate MUST be
// closed — if this host ever gains a runner, the gating assertions below are
// the ones to revisit.

// Fresh tour every test: the wardyn-onboarding-seen flag is what swaps the
// tour for the SetupScreen, and specs must not depend on each other's flags.
async function openSetupFunnel(page: import("@playwright/test").Page) {
  await gotoConsole(page);
  await navTo(page, "Getting started");
  // The welcome hero (onboarding tour) renders first on a fresh session.
  await expect(page.getByRole("button", { name: /get set up|finish setup/i })).toBeVisible();
  await page.getByRole("button", { name: /get set up|finish setup/i }).click();
  // The SetupScreen funnel replaces the hero.
  await expect(page.getByRole("heading", { name: "Getting started" })).toBeVisible();
  await expect(page.getByText(/step 1 of 9/i)).toBeVisible();
}

test.describe("Getting Started funnel", () => {
  test("walks all nine steps via Next in STEP_ORDER", async ({ page }) => {
    await openSetupFunnel(page);
    const main = page.getByRole("main");
    const nextBtn = page.getByRole("button", { name: /^Next:/i });

    // 1 environment
    await expect(main.getByRole("heading", { name: /pick your barrier/i })).toBeVisible();
    // 2 provider
    await nextBtn.click();
    await expect(
      main.getByRole("heading", { name: /connect a model or agent harness/i }),
    ).toBeVisible();
    // 3-5 your work: scm_provider → workspaces → credentials
    await nextBtn.click();
    await expect(main.getByRole("heading", { name: /source control provider/i })).toBeVisible();
    await nextBtn.click();
    await expect(main.getByRole("heading", { name: /onboard a workspace/i })).toBeVisible();
    await nextBtn.click();
    await expect(main.getByRole("heading", { name: /repo & cloud credentials/i })).toBeVisible();
    // 6-7 corporate network: host_proxy → artifact_repo
    await nextBtn.click();
    await expect(main.getByRole("heading", { name: /corporate host proxy/i })).toBeVisible();
    await nextBtn.click();
    await expect(
      main.getByRole("heading", { name: /artifact registry redirection/i }),
    ).toBeVisible();
    // 8-9 finish: review → launch
    await nextBtn.click();
    await expect(main.getByRole("heading", { name: /review readiness/i })).toBeVisible();
    await nextBtn.click();
    await expect(main.getByRole("heading", { name: /launch your first run/i })).toBeVisible();
    await expect(page.getByText(/step 9 of 9/i)).toBeVisible();
  });

  test("launch stays gated on a runner-less host (no fake green)", async ({ page }) => {
    await openSetupFunnel(page);
    const main = page.getByRole("main");

    // The environment step tells the truth about the seeded `none` runner.
    await expect(main.getByText(/no sandbox runner/i)).toBeVisible();
    // No fast-path banner — essentials (barrier + model) are not met.
    await expect(page.getByText(/you're ready — launch your first run now/i)).toHaveCount(0);

    // Footer on the last step: the launch button is disabled with the
    // essentials helper visible.
    const nextBtn = page.getByRole("button", { name: /^Next:/i });
    for (let i = 0; i < 8; i++) await nextBtn.click();
    await expect(main.getByRole("heading", { name: /launch your first run/i })).toBeVisible();
    for (const btn of await page.getByRole("button", { name: /launch your first run/i }).all()) {
      await expect(btn).toBeDisabled();
    }
    await expect(main.getByText(/a barrier and a connected model are both required/i)).toBeVisible();
  });

  test("Finish later dismisses to Runs and the sidebar entry returns", async ({ page }) => {
    await openSetupFunnel(page);
    await page.getByRole("button", { name: /finish later/i }).click();
    // Dismissal lands on Runs...
    await expect(page.getByRole("heading", { name: "Runs", level: 1 })).toBeVisible();
    // ...and Getting started remains reachable from the sidebar (now straight
    // to the funnel — the tour is one-shot).
    await navTo(page, "Getting started");
    await expect(page.getByRole("heading", { name: "Getting started" })).toBeVisible();
    await expect(page.getByText(/step 1 of 9/i)).toBeVisible();
  });
});
