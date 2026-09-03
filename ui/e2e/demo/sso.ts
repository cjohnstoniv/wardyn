/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * Signing in and out through Dex, for the multi-user episodes.
 *
 * The body below was written twice already — 04c-who-may-do-what.spec.ts:32
 * and 12b-admin-operations.spec.ts:48, byte for byte — and 04d needs it a
 * third time. It lives here rather than in demos.ts for the reason runs.ts
 * gives: demos.ts is in five graded episodes' import closures, and H6's
 * re-record rule would put all five back in front of the camera for a helper
 * none of them calls. 04c's and 12b's copies are deliberately LEFT ALONE for
 * the same reason — editing either spec re-opens a take that is already
 * approved. They retire into this module the next time either is re-shot.
 *
 * The password is deploy/kind/sso/README.md's demo literal, the same one the
 * two existing copies spell. It authenticates nobody outside the kind SSO
 * overlay's own throwaway Dex.
 */

import { type Page } from "@playwright/test";
import { act } from "./overlay";

/** deploy/kind/sso/README.md's demo literal — a throwaway Dex, no secret. */
const DEX_PASSWORD = "password";

/**
 * Walk the console's SSO button through Dex and back.
 *
 * The sign-in surface renders the entry point as a link on one lane and a
 * button on the other (sign-in.tsx has both), so it is addressed as either;
 * Dex's own login form is plain HTML with no accessible names worth speaking
 * of, hence the input-type locators.
 */
export async function dexSignIn(page: Page, email: string): Promise<void> {
  await page
    .getByRole("link", { name: "Sign in with SSO" })
    .or(page.getByRole("button", { name: "Sign in with SSO" }))
    .first()
    .click();
  await page.locator('input[type="password"]').waitFor({ timeout: 30_000 });
  await page.locator('input[type="text"], input[name="login"]').first().fill(email);
  await page.locator('input[type="password"]').fill(DEX_PASSWORD);
  await page.getByRole("button", { name: /log ?in/i }).click();
}

/**
 * Sign out ON CAMERA — the header's account menu, then Sign out.
 *
 * `line` is spoken on the menu-item click when the role flip is part of the
 * film (04c's shape); omitted, the gesture is silent (12b's).
 */
export async function dexSignOut(page: Page, line?: string): Promise<void> {
  await page.locator("header").getByRole("button").last().click();
  await act(
    page,
    page
      .getByRole("button", { name: "Sign out" })
      .or(page.getByRole("menuitem", { name: "Sign out" }))
      .first(),
    line,
  );
  await page.waitForTimeout(1500);
}
