/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, gotoConsole, sidebarLink } from "./fixtures";
import { PROVIDERS } from "../src/app/lib/workspace-providers-copy";
import { UNSAVED } from "../src/app/lib/unsaved-copy";
import type { Page } from "@playwright/test";

// #460 review round 2 — a real Chromium walk (not the vitest suite, which
// mocks history.go and so can't see either bug) reproduced two defects:
//
//   1. Back while dirty lost the draft 3/3: react-router's OWN popstate
//      listener (attached from a layout effect when <BrowserRouter> mounts)
//      saw every pop BEFORE this app's effect-based guard did, and had
//      already scheduled the popped route by the time the guard got a
//      chance to undo it — the dirty editor unmounted, taking its draft
//      with it. Fixed by installing the interception at MODULE scope
//      (use-unsaved-guard.tsx, evaluated on import — before <BrowserRouter>
//      exists to attach its own listener) and calling
//      event.stopImmediatePropagation() on a blocked pop, so react-router's
//      later-registered listener never runs for that event at all.
//   2. The skip link's fragment-only pop (history.state === null, since a
//      native `href="#main-content"` jump carries no react-router idx)
//      opened a false dialog and could leave the guard's bookkeeping
//      stuck, disarming the NEXT real Back.
//
// This spec drives the REAL app in a real browser — page.goBack() is a
// genuine browser Back, not a synthetic event — so both fixes are proven
// end to end, not just against a mocked history.go.

async function gotoDirtyProviders(page: Page): Promise<void> {
  // Every hop here is a REAL react-router navigation (a click, never a raw
  // pushState) — load-bearing for this spec: an untagged push (no `idx` on
  // its history.state, the shape navToRoute's own raw pushState leaves
  // behind) would never arm this guard's index tracking for the entry it
  // lands on, and a Back out of it would go unguarded regardless of what
  // this test types into the form.
  await gotoConsole(page);
  await sidebarLink(page, "Settings").click();
  await expect(page).toHaveURL(/\/settings$/);
  const card = page.getByTestId("providers-card");
  await expect(card).toBeVisible();
  await card.getByText(PROVIDERS.CARD_OPEN).click();
  await expect(page).toHaveURL(/\/providers$/);
  await expect(page.getByRole("heading", { name: PROVIDERS.TITLE, level: 1 })).toBeVisible();

  // The Storage tab's own Default disk field — dirtying this needs no
  // dependency on which git rows the seeded backend happens to carry.
  await page.getByRole("button", { name: PROVIDERS.STORAGE_TAB }).click();
  await page.getByLabel(PROVIDERS.FIELD_DEFAULT_DISK).fill("12345");
  await expect(page.getByTestId("unsaved-marker")).toBeVisible();
}

test.describe("unsaved guard — Back/Forward against the real app (#460 review round 2)", () => {
  test("dirty Providers, Back: restored and asked, draft intact; a second Back while the dialog is open stays put; Keep editing holds; Discard actually leaves", async ({
    page,
  }) => {
    await gotoDirtyProviders(page);

    // 1) Back while dirty: blocked, restored, asked — never a lost draft.
    await page.goBack();
    const dialog = page.getByRole("alertdialog");
    await expect(dialog).toBeVisible();
    await expect(dialog.getByText(UNSAVED.TITLE)).toBeVisible();
    await expect(page).toHaveURL(/\/providers$/);
    // The draft survived — this is the SAME mounted field, not a fresh one
    // reset to whatever loaded (a remount would read back "" or the
    // original value, never the typed one).
    await expect(page.getByLabel(PROVIDERS.FIELD_DEFAULT_DISK)).toHaveValue("12345");

    // 2) A second Back while the dialog is still open: also restored, not a
    // second stacked dialog, draft still intact.
    await page.goBack();
    await expect(page.getByRole("alertdialog")).toHaveCount(1);
    await expect(page).toHaveURL(/\/providers$/);
    await expect(page.getByLabel(PROVIDERS.FIELD_DEFAULT_DISK)).toHaveValue("12345");

    // 3) Keep editing: dialog closes, still here, draft still intact.
    await page.getByRole("button", { name: UNSAVED.STAY }).click();
    await expect(page.getByRole("alertdialog")).toHaveCount(0);
    await expect(page).toHaveURL(/\/providers$/);
    await expect(page.getByLabel(PROVIDERS.FIELD_DEFAULT_DISK)).toHaveValue("12345");

    // 4) The same Back, answered the other way: Discard genuinely leaves —
    // one real entry back, not a no-op.
    await page.goBack();
    await expect(page.getByRole("alertdialog")).toBeVisible();
    await page.getByRole("button", { name: UNSAVED.DISCARD }).click();
    await expect(page.getByRole("alertdialog")).toHaveCount(0);
    await expect(page).toHaveURL(/\/settings$/);
  });

  test("the skip link's fragment jump opens no dialog and moves focus to main; a real Back afterward is still guarded", async ({
    page,
  }) => {
    await gotoDirtyProviders(page);

    // The skip link is the FIRST focusable element in the shell — keyboard
    // activation (never a mouse click, which the sr-only styling makes
    // unreliable to target) is also the realistic path: a keyboard/screen-
    // reader user is exactly who this control is for.
    await page.getByRole("link", { name: "Skip to main content" }).focus();
    await page.keyboard.press("Enter");

    // No false dialog, and focus actually lands on the main region. Chromium
    // gives the fragment jump its own history entry (untagged: no
    // react-router idx), so the URL now carries the hash too.
    await expect(page.getByRole("alertdialog")).toHaveCount(0);
    await expect(page.locator("#main-content")).toBeFocused();
    await expect(page).toHaveURL(/\/providers#main-content$/);

    // Back off the fragment entry: it lands back on the SAME /providers
    // route (pathname unchanged, nothing ever unmounted) — correctly no
    // dialog, since nothing was actually at risk of being lost by undoing a
    // same-page scroll jump. What this proves is the OTHER half of the fix:
    // the fragment pop didn't corrupt the guard's bookkeeping into treating
    // this harmless pop as something it needed to intercept.
    await page.goBack();
    await expect(page.getByRole("alertdialog")).toHaveCount(0);
    await expect(page).toHaveURL(/\/providers$/);
    await expect(page.getByLabel(PROVIDERS.FIELD_DEFAULT_DISK)).toHaveValue("12345");

    // A SECOND real Back now genuinely tries to leave /providers — and that
    // one is still caught, proving the fragment pop never disarmed the guard
    // going forward.
    await page.goBack();
    await expect(page.getByRole("alertdialog")).toBeVisible();
    await expect(page.getByRole("alertdialog").getByText(UNSAVED.TITLE)).toBeVisible();
    await expect(page).toHaveURL(/\/providers$/);
  });
});
