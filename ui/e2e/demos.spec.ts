/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect } from "./fixtures";

// Demo sandboxes — hermetic walk against the seeded backend (real wardynd +
// Postgres + `none` runner, admin-token auth). The unit suites cover the catalog
// invariants and the card's start/poll logic; this spec proves the real wiring:
// the /demos catalog renders, and — because the seeded backend is `-runner none`
// — starting a demo is honestly GATED (disabled + hint). That gating IS the
// contract on this host; if it ever gains a runner, the last test is the one to
// revisit. (Demos live INSIDE Getting Started — the Demos phase, covered by
// getting-started.spec's step walk — and at /demos direct; the old Welcome-hero
// demo button + funnel intro link were removed with the mandatory setup gate.)

const DEMO_TITLES = [
  "The sealed box",
  "Fail, then approve",
  "Held at the door",
  "Lines that can't be crossed",
];

test.describe("Demo sandboxes", () => {
  test("the catalog renders all four demos", async ({ page }) => {
    await page.goto("/demos");
    await expect(page.getByRole("heading", { name: "Demo sandboxes" })).toBeVisible();
    for (const title of DEMO_TITLES) {
      await expect(page.getByRole("heading", { name: title })).toBeVisible();
    }
  });

  test("Start is gated on `-runner none`: disabled + honest hint", async ({ page }) => {
    await page.goto("/demos");
    await expect(page.getByRole("heading", { name: "Demo sandboxes" })).toBeVisible();
    // Browsing works, but no barrier is ready → every Start is closed, with a hint.
    await expect(page.getByTestId("demos-not-ready")).toBeVisible();
    const starts = page.getByRole("button", { name: /start demo/i });
    // The four keyless demos always render; the harness-aware fifth
    // ("agent-in-the-box", needsModel) appears only once a model is connected
    // (demo-screen filters on llmReady), so the visible count tracks model
    // readiness. Pin the invariant this test exists for instead — on a
    // runner-less host EVERY Start is closed, not just the first.
    const n = await starts.count();
    expect(n).toBeGreaterThanOrEqual(DEMO_TITLES.length);
    for (let i = 0; i < n; i++) {
      await expect(starts.nth(i)).toBeDisabled();
    }
  });
});
