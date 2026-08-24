/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect } from "./fixtures";

// Demo sandboxes — hermetic walk against the seeded backend (real wardynd +
// Postgres + `none` runner, admin-token auth). /demos DIED with the Getting
// Started consolidation (App.tsx redirects it into the funnel, replay
// 10a2e144); every demo is now a setup sub-step reachable at
// `/setup?step=<id>` (DemoDetail, setup/demos-step.tsx) — the deep-link
// corrector deliberately exempts demo steps from the corp_network gate
// (steps.ts's refuseSelect), so a bare `?step=` visit opens the demo
// directly, no bounce. The unit suites cover the catalog invariants and the
// card's start/poll logic; this spec proves the real wiring: the redirect
// lands where it says, each keyless demo renders at its deep link, and —
// because the seeded backend is `-runner none` — starting one is honestly
// GATED (disabled + hint). That gating IS the contract on this host; if it
// ever gains a runner, this is the spec to revisit.

const KEYLESS_DEMOS = [
  { id: "sealed-box", title: "The sealed box" },
  { id: "fail-then-approve", title: "Fail, then approve" },
  { id: "held-at-the-door", title: "Held at the door" },
  { id: "lines-that-cant-be-crossed", title: "Lines that can't be crossed" },
];

test.describe("Demo sandboxes", () => {
  test.beforeEach(async ({ page }) => {
    // /setup renders the WELCOME hero ("Run anything. Keep your keys.") until
    // this flag is set — the funnel (and so any demo step) only replaces it
    // once the operator clicks "Get started" (onboarding-screen.tsx's
    // GettingStarted). Pre-seed it so a deep link lands on the demo, not the
    // hero — same addInitScript pattern as the admin token in fixtures.ts /
    // docs.spec.ts's own theme+onboarding seed.
    await page.addInitScript(() => {
      try {
        localStorage.setItem("wardyn-onboarding-seen", "1");
      } catch {
        /* private mode — ignore */
      }
    });
  });

  test("/demos redirects into Getting Started", async ({ page }) => {
    await page.goto("/demos");
    await expect(page).toHaveURL(/\/setup\?step=sealed-box/);
  });

  for (const { id, title } of KEYLESS_DEMOS) {
    test(`${title} renders at its deep link, Start gated on -runner none`, async ({ page }) => {
      await page.goto(`/setup?step=${id}`);
      await expect(page.getByRole("heading", { name: title, level: 2 })).toBeVisible();
      // Browsing works, but no barrier is ready → Start is closed, with a hint.
      // The needsModel/needsSecret demos (agent-in-the-box, key-never-in-the-box,
      // authorized-not-issued) are NOT walked here: they're dropped from
      // stepOrder entirely without a connected model / stored secret — that
      // gating is deterministic unit coverage instead (steps.test.ts).
      await expect(page.getByTestId("demos-step-not-ready")).toBeVisible();
      await expect(page.getByTestId(`demo-start-${id}`)).toBeDisabled();
    });
  }
});
