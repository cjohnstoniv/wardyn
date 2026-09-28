/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, asRealMember, gotoConsole, navToRoute } from "./fixtures";

// #1200 compact cards — the owner measured Admin Settings at 2625px and Your
// account at 981px against a 744px viewport with every card fully open
// (settings-compact-1200-packet.html). Every card on both pages now collapses
// to a one-line summary by default (collapsible-card.tsx) — this is the pin
// that proves the fix at the real backend, not just in a unit test: at
// 1280x744, neither page's <main> (the shell's own scroll container,
// app-shell.tsx) scrolls once its cards have loaded, collapsed.
//
// Mutation proof (recorded here, not re-run by CI): defaulting HostCard's
// CollapsibleCard to `defaultOpen` — the single largest card, carrying the
// barrier picker, the Image builder/Recording store/Internet rows and the
// Corporate proxy button — pushes Admin Settings' content past 744px on its
// own, and this test fails (main.scrollHeight > main.clientHeight) exactly as
// it must. Reverting the mutation restores the pass. This is the sharpest
// case: Admin Settings' six other cards collapsed still fit comfortably
// beneath 744px, so Host opening alone is what tips it over.
const VIEWPORT = { width: 1280, height: 744 };

async function mainScrolls(page: import("@playwright/test").Page): Promise<boolean> {
  return page.evaluate(() => {
    const m = document.querySelector("main");
    return !!m && m.scrollHeight > m.clientHeight;
  });
}

test.describe("Settings and Your account fit 1280x744 collapsed (#1200 compact cards)", () => {
  test("Admin Settings: every card collapsed, no scroll on main", async ({ page }) => {
    await page.setViewportSize(VIEWPORT);
    await gotoConsole(page, "admin");
    await navToRoute(page, "/admin/settings");

    // Loaded: the last card (Admin SSH keys) is the surest "the page finished
    // rendering every card" signal — earlier cards could be present while a
    // later one is still pending its own fetch.
    await expect(page.getByRole("heading", { name: "Admin SSH keys" })).toBeVisible();

    // Collapsed by default: none of the seven cards' bodies are in the DOM.
    for (const title of ["Host", "Branding", "Model providers", "Model provider", "Workspace providers", "User drives", "Admin SSH keys"]) {
      const toggle = page.getByRole("button", { name: new RegExp(`^${title}( |$)`) });
      await expect(toggle).toHaveAttribute("aria-expanded", "false");
    }

    expect(await mainScrolls(page)).toBe(false);
  });

  test("Your account: every card collapsed, no scroll on main", async ({ page }) => {
    await page.setViewportSize(VIEWPORT);
    await asRealMember(page);
    await gotoConsole(page);
    await navToRoute(page, "/account");

    await expect(page.getByRole("heading", { name: "Your SSH keys", level: 3 })).toBeVisible();

    for (const title of ["Model provider", "Your SSH keys"]) {
      const toggle = page.getByRole("button", { name: new RegExp(`^${title}( |$)`) });
      await expect(toggle).toHaveAttribute("aria-expanded", "false");
    }

    expect(await mainScrolls(page)).toBe(false);
  });

  // Expanding every card at once is the honest worst case a real person can
  // reach (nothing stops opening them all) — this is NOT the 744px promise
  // (only collapsed is), but it must still resolve to a real, scrollable page
  // rather than a broken layout, and no card may vanish or throw once opened.
  test("Admin Settings: expanding every card in turn never loses a card or throws", async ({ page }) => {
    await page.setViewportSize(VIEWPORT);
    await gotoConsole(page, "admin");
    await navToRoute(page, "/admin/settings");
    await expect(page.getByRole("heading", { name: "Admin SSH keys" })).toBeVisible();

    for (const title of ["Host", "Branding", "Model providers", "Model provider", "Workspace providers", "User drives", "Admin SSH keys"]) {
      const toggle = page.getByRole("button", { name: new RegExp(`^${title}( |$)`) });
      await toggle.click();
      await expect(toggle).toHaveAttribute("aria-expanded", "true");
    }
    // Every heading is still there — expanding one card never displaced or
    // unmounted another.
    for (const title of ["Host", "Branding", "Model providers", "Model provider", "Workspace providers", "User drives", "Admin SSH keys"]) {
      await expect(page.getByRole("heading", { name: title, exact: true })).toBeVisible();
    }
  });
});
