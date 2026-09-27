/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect } from "./fixtures";

// Demo sandboxes — hermetic walk against the seeded backend (real wardynd +
// Postgres + `none` runner, admin-token auth). /demos DIED with the Getting
// Started consolidation (App.tsx redirects it into the funnel, replay
// 10a2e144); M-6 (D5, admin-member-modes-design.md §4.8) then moved every
// demo OUT of the admin funnel and into User Getting Started
// (member-getting-started.tsx) — a demo is a sandbox run, a user act. Each
// one is still reachable at `/setup?step=<id>` (App.tsx's /demos redirect is
// unchanged), but the id now opens a ROW on that page (DemoRow), not a
// rail-driven single-step view — there is no funnel here any more, no "Step N
// of M", no per-demo <h2>. This backend's admin-token auth is a
// single-operator (D1) principal, so /setup shows this page for it too — the
// URL alone decides the view (console-view.tsx). The unit suites cover the
// catalog invariants and the card's start/poll logic; this spec proves the
// real wiring: the redirect lands where it says, each keyless demo's row
// opens at its deep link, and — because the seeded backend is `-runner
// none` — starting one is honestly GATED (disabled + hint). That gating IS
// the contract on this host; if it ever gains a runner, this is the spec to
// revisit.

// Demos this backend can honestly deep-link to: no model, no stored secret.
// The last two are the per-KIND cards that gate on NEITHER — github-app-broker
// is TEACH+GATE (it keeps its step and closes its Start, so a missing App does
// not delete it from the walk) and sts-fail-closed is keyless by construction.
const KEYLESS_DEMOS = [
  { id: "sealed-box", title: "The sealed box" },
  { id: "fail-then-approve", title: "Fail, then approve" },
  { id: "held-at-the-door", title: "Held at the door" },
  { id: "lines-that-cant-be-crossed", title: "Lines that can't be crossed" },
  { id: "denied-however-spelled", title: "Denied, however you spell it" },
  { id: "github-app-broker", title: "A token the sandbox never even sees" },
  { id: "sts-fail-closed", title: "No identity, no credential" },
];

test.describe("Demo sandboxes", () => {
  test.beforeEach(async ({ page }) => {
    // /setup renders the WELCOME hero ("Sandboxed. Governed. Self-hosted. Free.") until
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
      // The row's own title (member-getting-started.tsx's DemoRow), pre-opened
      // by the ?step= deep link — never a level-2 heading, which was the
      // funnel step's own SetupLayout chrome, gone with the funnel.
      await expect(page.getByText(title, { exact: true })).toBeVisible();
      await expect(page.getByTestId(`demo-card-${id}`)).toBeVisible();
      // Browsing works, but no barrier is ready → Start is closed, with a hint.
      // The needsModel/needsSecret demos (agent-in-the-box, plus the five that
      // name a stored secret) are NOT walked here: walkableDemos (steps.ts)
      // drops them entirely without a connected model / stored secret — that
      // gating is deterministic unit coverage instead (steps.test.ts). The
      // needsGitHubApp one is walked precisely BECAUSE it is not dropped.
      await expect(page.getByTestId("demos-step-not-ready")).toBeVisible();
      await expect(page.getByTestId(`demo-start-${id}`)).toBeDisabled();
    });
  }
});
