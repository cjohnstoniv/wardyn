/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, gotoConsole, navTo } from "./fixtures";

// Demo sandboxes — hermetic walk against the seeded backend (real wardynd +
// Postgres + `none` runner, admin-token auth). The unit suites cover the catalog
// invariants and the card's start/poll logic; this spec proves the real wiring:
// the Welcome hero CTA and the funnel intro both reach /demos, the catalog
// renders, and — because the seeded backend is `-runner none` — starting a demo
// is honestly GATED (disabled + hint). That gating IS the contract on this host;
// if it ever gains a runner, the last test is the one to revisit.

const DEMO_TITLES = [
  "The sealed box",
  "Fail, then approve",
  "Held at the door",
  "Lines that can't be crossed",
];

test.describe("Demo sandboxes", () => {
  test("the Welcome hero CTA reaches /demos", async ({ page }) => {
    await gotoConsole(page);
    await navTo(page, "Getting started");
    // Fresh session → the welcome hero renders before the funnel.
    const cta = page.getByRole("button", { name: /try a 2-minute demo sandbox/i });
    await expect(cta).toBeVisible();
    await cta.click();
    await expect(page).toHaveURL(/\/demos$/);
    await expect(page.getByRole("heading", { name: "Demo sandboxes" })).toBeVisible();
  });

  test("the catalog renders all four demos", async ({ page }) => {
    await page.goto("/demos");
    await expect(page.getByRole("heading", { name: "Demo sandboxes" })).toBeVisible();
    for (const title of DEMO_TITLES) {
      await expect(page.getByRole("heading", { name: title })).toBeVisible();
    }
  });

  test("the setup funnel intro links to the demos", async ({ page }) => {
    await gotoConsole(page);
    await navTo(page, "Getting started");
    // Advance past the welcome hero into the funnel shell.
    await page.getByRole("button", { name: /get set up|finish setup/i }).click();
    await expect(page.getByRole("heading", { name: "Getting started" })).toBeVisible();
    // The intro panel is dismissible — open it, then the demo link is there.
    await page.getByRole("button", { name: /show intro/i }).click();
    const link = page.getByRole("link", { name: /run a demo sandbox first/i });
    await expect(link).toBeVisible();
    await expect(link).toHaveAttribute("href", "/demos");
  });

  test("Start is gated on `-runner none`: disabled + honest hint", async ({ page }) => {
    await page.goto("/demos");
    await expect(page.getByRole("heading", { name: "Demo sandboxes" })).toBeVisible();
    // Browsing works, but no barrier is ready → every Start is closed, with a hint.
    await expect(page.getByTestId("demos-not-ready")).toBeVisible();
    const starts = page.getByRole("button", { name: /start demo/i });
    await expect(starts).toHaveCount(4);
    await expect(starts.first()).toBeDisabled();
  });
});
