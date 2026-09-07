/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, gotoConsole, mockMemberRole, mockSecurityAdminRole, navToRoute } from "./fixtures";

// Member Getting Started (Phase 5) — same mockMemberRole splice
// member-console.spec.ts uses (the seeded backend always authenticates as
// admin server-side; only the CLIENT believes it is a member). This spec
// proves the render: the six member sections replace the operator funnel at
// /setup, the account menu drops Demos, and the video player streams nothing
// until Watch is pressed. Server-side ownership scoping (creator-scoped
// runs/secrets) is proven in Go, not here — the daemon-backed part of the
// plan's invariant.

const MEMBER_SECTION_TITLES = [
  "What's set up for you",
  "Add your workspace",
  "Your model key",
  "Your first run",
  "Approvals you can decide",
  "Connect your tools",
] as const;

test.describe("member Getting Started (mocked /me role)", () => {
  test.beforeEach(async ({ page }) => {
    await mockMemberRole(page);
  });

  test("shows the six member sections, never the admin funnel", async ({ page }) => {
    await gotoConsole(page);
    await navToRoute(page, "/setup");

    for (const title of MEMBER_SECTION_TITLES) {
      await expect(page.getByRole("heading", { name: title })).toBeVisible();
    }
    await expect(page.getByText("Setup is managed by your workspace admin")).toHaveCount(0);
    await expect(page.getByText("Pick your barrier")).toHaveCount(0);
  });

  test("the episode rail leads with the member's own path, then core (Shape C)", async ({
    page,
  }) => {
    await gotoConsole(page);
    await navToRoute(page, "/setup");
    // Group labels, in order: "Your path" (every member-audience episode —
    // including 13, whose lesson is the member terminal) then "Start here".
    const labels = page.locator(".label-eyebrow", { hasText: /Your path|Start here/ });
    await expect(labels).toHaveCount(2);
    await expect(labels.nth(0)).toHaveText("Your path");
    await expect(labels.nth(1)).toHaveText("Start here");
    await expect(page.getByText("Your terminal, our cluster")).toBeVisible();
    // The flipped core episode is in the member's Start-here set.
    await expect(page.getByText("Your first policy")).toBeVisible();
  });

  test("the account menu has no Demos entry", async ({ page }) => {
    await gotoConsole(page);
    await page.locator("header").getByRole("button").last().click();
    const menu = page.getByRole("menu");
    await expect(menu).toBeVisible();
    await expect(menu.getByText("Demos")).toHaveCount(0);
  });

  test("an episode streams nothing until Watch is pressed, then at least one request", async ({ page }) => {
    let mp4Requests = 0;
    await page.route("**/*.mp4", async (route) => {
      mp4Requests++;
      await route.fulfill({ status: 200, contentType: "video/mp4", body: Buffer.alloc(16) });
    });

    await gotoConsole(page);
    await navToRoute(page, "/setup");
    await expect(page.getByRole("heading", { name: "What's set up for you" })).toBeVisible();

    // Rows finish rendering before any network activity — no prefetch.
    const watch = page.getByRole("button", { name: "Watch" }).first();
    await expect(watch).toBeVisible();
    expect(mp4Requests).toBe(0);

    await watch.click();
    await expect.poll(() => mp4Requests, { timeout: 5000 }).toBeGreaterThanOrEqual(1);
  });
});

// Sibling negative control: the SAME route, unspliced (the harness's real
// admin session) — the operator funnel, the Demos entry, and none of the
// member-only section titles.
test.describe("admin session at /setup (unmocked — negative control)", () => {
  test.beforeEach(async ({ page }) => {
    // Past the welcome hero, straight to the funnel's own step heading — same
    // seed demos.spec.ts uses for a deep link into /setup.
    await page.addInitScript(() => {
      try {
        localStorage.setItem("wardyn-onboarding-seen", "1");
      } catch {
        /* private mode — ignore */
      }
    });
  });

  test("the admin still sees the barrier step, Demos, and no member sections", async ({ page }) => {
    await gotoConsole(page);
    await navToRoute(page, "/setup");

    await expect(page.getByRole("heading", { name: "Pick your barrier" })).toBeVisible();

    // /setup renders the funnel's own <header> inside the shell, so scope to the
    // shell's top bar (the first header in DOM order) before taking the last
    // button — the account-menu trigger. An unscoped .last() lands on a funnel
    // button and no menu ever opens.
    await page.locator("header").first().getByRole("button").last().click();
    const menu = page.getByRole("menu");
    await expect(menu.getByText("Demos")).toBeVisible();

    for (const title of MEMBER_SECTION_TITLES) {
      await expect(page.getByRole("heading", { name: title })).toHaveCount(0);
    }
  });
});

// R4/F034: the guard in GettingStarted was `role === "member"` after role went
// three-valued, so a SECURITY ADMIN fell through to the deployer funnel — built
// from a SetupStatus the server redacts for every non-operator
// (redactSetupStatusForMember zeroes Checks/Providers/Secrets,
// internal/api/setup.go), over mutations that are super-admin-only. It reads
// `role !== "admin"` now, the way setupGateActive already did.
//
// Browser-only: this is a ROUTE decision made from /me, so only a real
// navigation with a security-admin /me proves it.
test.describe("security admin at /setup (mocked /me role) — R4/F034", () => {
  test.beforeEach(async ({ page }) => {
    await mockSecurityAdminRole(page);
  });

  test("lands on the member Getting Started, never the deployer funnel", async ({ page }) => {
    await gotoConsole(page);
    await navToRoute(page, "/setup");

    for (const title of MEMBER_SECTION_TITLES) {
      await expect(page.getByRole("heading", { name: title })).toBeVisible();
    }
    // The operator welcome hero and the funnel's barrier picker — the two
    // surfaces built from the redacted status.
    await expect(page.getByText("Sandboxed. Governed. Self-hosted. Free.")).toHaveCount(0);
    await expect(page.getByText("Pick your barrier")).toHaveCount(0);
  });
});
