/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, asRealMember, gotoConsole, navToRoute } from "./fixtures";

// #1200 compact cards — the owner measured Admin Settings at 2625px and Your
// account at 981px against a 744px viewport with every card fully open
// (settings-compact-1200-packet.html). Every card on both pages now collapses
// to a one-line summary by default (collapsible-card.tsx) — this is the pin
// that proves the fix at the real backend, not just in a unit test: at its
// viewport budget (Your account 1280x744, Admin Settings below), neither
// page's <main> (the shell's own scroll container, app-shell.tsx) scrolls once
// its cards have loaded, collapsed. Admin Settings no longer fits 744: the
// group headings and the notice cost it more than that.
//
// Mutation proof, kept as a test below: opening Host (the largest card, with the barrier picker, the Image
// builder/Recording store/Internet rows and the Corporate proxy button) must push Admin Settings past its budget,
// so a budget loosened until that stops being true fails there. Opening the card in the UI is the same change as
// defaulting HostCard's CollapsibleCard to `defaultOpen`.
const VIEWPORT = { width: 1280, height: 744 };
// Admin Settings (three group headings, eight collapsed cards, the no-barrier notice) needs 852px of viewport,
// measured on the hosted runner at 1280 wide: 115px of chrome around <main> plus 737px of content. Collapsed
// Host opened needs 1241px. The budget is the measured height plus a small margin; TOLERANCE_PX below is the
// sub-pixel jitter allowance on top.
const ADMIN_VIEWPORT = { width: 1280, height: 860 };

// runs.spec.ts's own "the page itself does not scroll" check notes the same
// class of issue ("CI-flake: measured 300.28px ... sub-pixel layout jitter,
// not a real regression ... it would not tolerate a genuine multi-hundred-
// pixel regression, which this still catches"). Here that jitter is
// cross-environment line-height rounding on the collapsed card row (measured
// 65.875px here against the 63.9px waitForSettledSummaries' own comment
// below records) — compounded over Admin Settings' eight cards, a couple of
// px each. TOLERANCE_PX is nowhere near what a real defect produces: the
// mutation proof below (Host defaulting open) adds ~200px on its own.
const TOLERANCE_PX = 24;

async function mainScrolls(page: import("@playwright/test").Page): Promise<boolean> {
  return page.evaluate((tolerance) => {
    const m = document.querySelector("main");
    return !!m && m.scrollHeight > m.clientHeight + tolerance;
  }, TOLERANCE_PX);
}

// review M2 — Model providers, Workspace providers, User drives, Admin SSH
// keys and Your SSH keys each fetch their OWN summary after the page's own
// heading is already on screen (a "the last card's heading is visible" wait
// alone catches the page's first paint, not each card's own settled fetch).
// A probe against this same backend measured a collapsed card at 44px before
// its summary lands and 63.9px after — up to ~40px of understatement, which
// the height budget (a few dozen px of headroom) cannot absorb silently.
// Waiting for each card's REAL summary text (never the empty pre-fetch
// button name, which still matches on title alone) is what makes the
// measurement below a steady-state one.
async function waitForSettledSummaries(page: import("@playwright/test").Page, patterns: RegExp[]) {
  for (const pattern of patterns) {
    // A card that only links out (#3048) is a link row, not an accordion button.
    await expect(page.getByRole("button", { name: pattern }).or(page.getByRole("link", { name: pattern }))).toBeVisible();
  }
}

test.describe("Settings and Your account fit their viewport budget collapsed (#1200 compact cards)", () => {
  test("Admin Settings: every card collapsed, no scroll on main", async ({ page }) => {
    await page.setViewportSize(ADMIN_VIEWPORT);
    await gotoConsole(page, "admin");
    await navToRoute(page, "/admin/settings");

    // Loaded: the last card (Admin SSH keys) is the surest "the page finished
    // rendering every card" signal — earlier cards could be present while a
    // later one is still pending its own fetch.
    await expect(page.getByRole("heading", { name: "Admin SSH keys" })).toBeVisible();
    // Settled (review M2): each self-fetching card's own summary, not just
    // its heading.
    await waitForSettledSummaries(page, [
      /^Model providers \S/,
      /^Workspace providers \S/,
      /^Custom components \S/,
      /^User drives \S/,
      /^Admin SSH keys \d+ admin key/,
      /^SCIM provisioning\s*\S/,
      /^Approval notifications \S/,
    ]);

    // Collapsed by default: none of the nine cards' bodies are in the DOM.
    for (const title of ["Host", "Branding", "Model providers", "Custom components", "Admin SSH keys", "SCIM provisioning", "Approval notifications"]) {
      const toggle = page.getByRole("button", { name: new RegExp(`^${title}( |$)`) });
      await expect(toggle).toHaveAttribute("aria-expanded", "false");
    }
    // The two cards that only link elsewhere are rows with a link, never an accordion (#3048).
    for (const [title, href] of [["Workspace providers", "/admin/providers"], ["User drives", "/admin/drives"]]) {
      await expect(page.getByRole("link", { name: new RegExp(`^${title}( |$)`) })).toHaveAttribute("href", new RegExp(`${href}$`));
    }

    expect(await mainScrolls(page)).toBe(false);
  });

  // The mutation proof: Host open (the largest card) must break the budget above. If the budget is ever loosened
  // until this fails, the collapsed-by-default promise has stopped meaning anything.
  test("Admin Settings: the Host card open breaks the budget", async ({ page }) => {
    await page.setViewportSize(ADMIN_VIEWPORT);
    await gotoConsole(page, "admin");
    await navToRoute(page, "/admin/settings");
    await expect(page.getByRole("heading", { name: "Admin SSH keys" })).toBeVisible();
    await waitForSettledSummaries(page, [/^Admin SSH keys \d+ admin key/, /^SCIM provisioning\s*\S/, /^Model providers \S/]);
    await page.getByRole("button", { name: /^Host( |$)/ }).click();
    expect(await mainScrolls(page)).toBe(true);
  });

  test("Your account: every card collapsed, no scroll on main", async ({ page }) => {
    await page.setViewportSize(VIEWPORT);
    await asRealMember(page);
    await gotoConsole(page);
    await navToRoute(page, "/account");

    await expect(page.getByRole("heading", { name: "Your SSH keys", level: 3 })).toBeVisible();
    // Settled (review M2): Your SSH keys fetches its own list separately
    // from the page's initial status load.
    await waitForSettledSummaries(page, [/^Your SSH keys (No keys|\d+ keys?)/]);

    for (const title of ["Your SSH keys"]) {
      const toggle = page.getByRole("button", { name: new RegExp(`^${title}( |$)`) });
      await expect(toggle).toHaveAttribute("aria-expanded", "false");
    }

    expect(await mainScrolls(page)).toBe(false);
  });

  // Expanding every card at once is the honest worst case a real person can
  // reach (nothing stops opening them all) — this is NOT the viewport promise
  // (only collapsed is), but it must still resolve to a real, scrollable page
  // rather than a broken layout, and no card may vanish or throw once opened.
  test("Admin Settings: expanding every card in turn never loses a card or throws", async ({ page }) => {
    await page.setViewportSize(ADMIN_VIEWPORT);
    await gotoConsole(page, "admin");
    await navToRoute(page, "/admin/settings");
    await expect(page.getByRole("heading", { name: "Admin SSH keys" })).toBeVisible();

    for (const title of ["Host", "Branding", "Model providers", "Admin SSH keys", "SCIM provisioning", "Approval notifications"]) {
      const toggle = page.getByRole("button", { name: new RegExp(`^${title}( |$)`) });
      await toggle.click();
      await expect(toggle).toHaveAttribute("aria-expanded", "true");
    }
    // Every heading is still there — expanding one card never displaced or
    // unmounted another. A prefix regex, not a plain substring (review
    // R2-M1): the heading's own name is now title+summary, same as the
    // button's, since it carries no `aria-label` override, and a plain
    // substring match on "Model provider" would also hit "Model providers"'
    // own heading, or on "Model providers" would also hit its expanded
    // body's unrelated `<h4>` empty-state heading ("No model providers
    // yet").
    for (const title of ["Host", "Branding", "Model providers", "Workspace providers", "User drives", "Admin SSH keys", "SCIM provisioning", "Approval notifications"]) {
      await expect(page.getByRole("heading", { name: new RegExp(`^${title}( |$)`), level: 3 })).toBeVisible();
    }
  });
});
