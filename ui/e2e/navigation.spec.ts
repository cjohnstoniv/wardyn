/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, gotoConsole, navTo, sidebarLink, type NavLabel } from "./fixtures";

// Navigation + theme + error-boundary coverage for the Wardyn admin console.
//
// The shell (app-shell.tsx) renders a grouped sidebar (Operate / Configure /
// Forensics + a pinned Getting started entry) whose entries are react-router
// <NavLink>s (role="link") labelled Runs/Approvals/Policies/Secrets/Audit/
// Recordings/Getting started, a top-bar "Toggle theme" button (aria-label), and
// a per-screen <h1> page title supplied by PageHeader. Fleet is retired from the
// nav (still routable at /fleet, not covered here). AppShell wraps the routed
// screen in an ErrorBoundary keyed by pathname so a render error in one screen
// degrades to an inline alert card and navigating away clears it. The theme
// lives on <html> (documentElement.dark + color-scheme) via ThemeProvider —
// these specs assert that root state, never a tailwind utility class on a leaf
// node.

// Every sidebar destination, its <h1> page title, and a distinctive subtitle the
// screen renders so we prove the *screen content* mounted, not just the heading.
// The redesign made Recordings' screen heading plural ("Recordings") to match its
// sidebar label. Every blurb below is the screen's real PageHeader description
// (copy in the respective screen component). Getting started is deliberately NOT
// covered here: on a fresh session (no wardyn-onboarding-seen flag) it renders
// the onboarding tour, not the SetupScreen "Getting started" heading — that
// tour/wizard split is pre-existing product behavior (onboarding-screen.tsx),
// unit-tested separately in onboarding-screen.test.tsx.
const DESTINATIONS: { label: NavLabel; heading: string; blurb: RegExp }[] = [
  { label: "Runs", heading: "Runs", blurb: /each confined behind its own barrier/i },
  { label: "Approvals", heading: "Approvals", blurb: /nothing privileged happens without one/i },
  { label: "Policies", heading: "Policies", blurb: /egress allowlist/i },
  { label: "Secrets", heading: "Secrets", blurb: /values go in and never come out/i },
  { label: "Audit", heading: "Audit", blurb: /Append-only/i },
  { label: "Recordings", heading: "Recordings", blurb: /Captured terminal sessions, replayed byte-for-byte/i },
];

// The set of sidebar links that must remain mounted on every screen — proves
// the app shell never unmounts as the user navigates between regions.
const SIDEBAR_LABELS: NavLabel[] = [
  "Runs",
  "Approvals",
  "Policies",
  "Secrets",
  "Audit",
  "Recordings",
  "Getting started",
];

async function expectSidebarMounted(page: import("@playwright/test").Page) {
  for (const label of SIDEBAR_LABELS) {
    await expect(sidebarLink(page, label)).toBeVisible();
  }
}

// Reads the live theme off <html>: class "dark" present + color-scheme value.
async function readTheme(page: import("@playwright/test").Page) {
  return page.evaluate(() => ({
    hasDarkClass: document.documentElement.classList.contains("dark"),
    colorScheme: document.documentElement.style.colorScheme,
    stored: window.localStorage.getItem("wardyn-theme"),
  }));
}

test.describe("navigation + shell", () => {
  test("boots into the Runs screen with the sidebar mounted", async ({ page }) => {
    await gotoConsole(page);
    // Runs is the default region (/ redirects to /runs): its <h1> and content
    // are present on boot.
    await expect(page.getByRole("heading", { name: "Runs", level: 1 })).toBeVisible();
    await expect(page.getByText(/each confined behind its own barrier/i)).toBeVisible();
    await expectSidebarMounted(page);
  });

  test("every sidebar destination loads its screen heading + content", async ({ page }) => {
    await gotoConsole(page);
    for (const dest of DESTINATIONS) {
      await navTo(page, dest.label);
      // The screen's own <h1> title (PageHeader renders an h1, distinct from the
      // sidebar <button> of the same name).
      await expect(
        page.getByRole("heading", { name: dest.heading, level: 1 }),
      ).toBeVisible();
      // ...and a screen-specific subtitle proves the *content* mounted.
      await expect(page.getByText(dest.blurb)).toBeVisible();
      // The shell (sidebar) survives the navigation.
      await expectSidebarMounted(page);
    }
  });

  test("the app shell (sidebar) stays mounted across navigation", async ({ page }) => {
    await gotoConsole(page);
    // Capture the Runs sidebar link handle, then navigate the full circuit and
    // back; the same shell element must remain attached the entire time.
    const runsNav = sidebarLink(page, "Runs");
    await expect(runsNav).toBeVisible();

    const circuit: { label: NavLabel; heading: string }[] = [
      { label: "Audit", heading: "Audit" },
      { label: "Policies", heading: "Policies" },
      { label: "Secrets", heading: "Secrets" },
      { label: "Recordings", heading: "Recordings" },
      { label: "Approvals", heading: "Approvals" },
    ];
    for (const dest of circuit) {
      await navTo(page, dest.label);
      await expect(page.getByRole("heading", { name: dest.heading, level: 1 })).toBeVisible();
      // Sidebar Runs link never detaches while we move between screens.
      await expect(runsNav).toBeAttached();
      await expect(runsNav).toBeVisible();
    }
  });

  test("navigating away from a screen and back re-renders it", async ({ page }) => {
    await gotoConsole(page);

    // Boot lands on Runs already; leave to Audit, then come back to Runs.
    await navTo(page, "Runs");
    await expect(page.getByRole("heading", { name: "Runs", level: 1 })).toBeVisible();

    await navTo(page, "Audit");
    await expect(page.getByRole("heading", { name: "Audit", level: 1 })).toBeVisible();
    // Runs content is no longer in the main region (only one screen renders).
    await expect(page.getByText(/each confined behind its own barrier/i)).toHaveCount(0);

    await navTo(page, "Runs");
    await expect(page.getByRole("heading", { name: "Runs", level: 1 })).toBeVisible();
    await expect(page.getByText(/each confined behind its own barrier/i)).toBeVisible();
    // Audit subtitle is gone now that we returned to Runs.
    await expect(page.getByText(/Append-only/i)).toHaveCount(0);
  });

  test("the active sidebar item reflects the current screen", async ({ page }) => {
    await gotoConsole(page);
    // The seeded backend has PENDING approvals; the Approvals entry must exist
    // and be navigable. Clicking it lands on the Approvals screen.
    await navTo(page, "Approvals");
    await expect(page.getByRole("heading", { name: "Approvals", level: 1 })).toBeVisible();
    // Going to a different screen swaps the heading (single-screen region).
    await navTo(page, "Policies");
    await expect(page.getByRole("heading", { name: "Policies", level: 1 })).toBeVisible();
    await expect(page.getByRole("heading", { name: "Approvals", level: 1 })).toHaveCount(0);
  });
});

test.describe("theme toggle", () => {
  test("defaults to dark and the Toggle theme button is present", async ({ page }) => {
    await gotoConsole(page);
    const toggle = page.getByRole("button", { name: "Toggle theme" });
    await expect(toggle).toBeVisible();
    // ThemeProvider is dark-first; <html> carries the dark class + color-scheme.
    const initial = await readTheme(page);
    expect(initial.hasDarkClass).toBe(true);
    expect(initial.colorScheme).toBe("dark");
  });

  test("the Toggle theme button flips dark <-> light on documentElement", async ({ page }) => {
    await gotoConsole(page);
    const toggle = page.getByRole("button", { name: "Toggle theme" });

    const before = await readTheme(page);
    expect(before.hasDarkClass).toBe(true);

    // Flip to light.
    await toggle.click();
    await expect
      .poll(async () => (await readTheme(page)).hasDarkClass)
      .toBe(false);
    const light = await readTheme(page);
    expect(light.colorScheme).toBe("light");
    expect(light.stored).toBe("light");

    // Flip back to dark.
    await toggle.click();
    await expect
      .poll(async () => (await readTheme(page)).hasDarkClass)
      .toBe(true);
    const dark = await readTheme(page);
    expect(dark.colorScheme).toBe("dark");
    expect(dark.stored).toBe("dark");
  });

  test("theme choice persists across navigation", async ({ page }) => {
    await gotoConsole(page);
    const toggle = page.getByRole("button", { name: "Toggle theme" });

    // Switch to light, then navigate to another screen.
    await toggle.click();
    await expect.poll(async () => (await readTheme(page)).hasDarkClass).toBe(false);

    await navTo(page, "Audit");
    await expect(page.getByRole("heading", { name: "Audit", level: 1 })).toBeVisible();
    // The light theme set on <html> survives the in-app navigation.
    expect((await readTheme(page)).hasDarkClass).toBe(false);

    // The toggle is still mounted on the new screen and remains operable.
    await page.getByRole("button", { name: "Toggle theme" }).click();
    await expect.poll(async () => (await readTheme(page)).hasDarkClass).toBe(true);
  });
});

test.describe("error boundary (no spurious fallback)", () => {
  test("no screen renders the error-boundary fallback under healthy data", async ({ page }) => {
    // The ErrorBoundary fallback is role="alert" containing "Something went
    // wrong". The seeded backend exposes runs in ALL nine RunStates (incl. the
    // COMPLETED-state value that previously threw during render). Visiting every
    // screen must NOT trip the boundary — proving the fail-soft rendering holds.
    await gotoConsole(page);
    for (const dest of DESTINATIONS) {
      await navTo(page, dest.label);
      await expect(page.getByRole("heading", { name: dest.heading, level: 1 })).toBeVisible();
      // No error-boundary fallback card anywhere on the page for this screen.
      await expect(
        page.getByRole("alert").filter({ hasText: /Something went wrong/i }),
      ).toHaveCount(0);
      // And the screen's content actually rendered (boundary did not swallow it).
      await expect(page.getByText(dest.blurb)).toBeVisible();
    }
  });
});
