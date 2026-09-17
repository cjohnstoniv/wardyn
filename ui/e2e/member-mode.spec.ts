/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, gotoConsole, sidebarLink } from "./fixtures";
import { MEMBER_MODE } from "../src/app/components/wardyn/member-mode-banner";

// "View as member" (0.7.4, P2) — the CLIENT half.
//
// The seeded e2e backend authenticates every spec with a bare admin bearer
// token, and isOperator reads "no session role to demote" for a caller with no
// OIDC human session — so POST /me/member-mode answers 400 here by design, and
// there is no genuine member-mode session to reach through this harness. The
// /me splice is the same mockMemberRole idiom fixtures.ts documents (route.fetch
// + patch + refulfill), extended to the new field, and the POST is intercepted
// so the toggle's REQUEST BODY is asserted rather than its server effect.
//
// The FULL round trip on two real OIDC identities — real toggle, /me
// operator:false, an operator-only write actually 403ing, Exit restoring admin —
// is the kind-sso walk (lane sso-test-path, W5), not this file. That walk is the
// only place a real session cookie exists.

/** Splices member_mode (and the tier fields the server clamps with it) onto the
 *  real /me, so the console believes it is an admin in member mode. */
async function mockMemberMode(
  page: import("@playwright/test").Page,
  noCredential = false,
): Promise<void> {
  await page.route("**/api/v1/me", async (route) => {
    const response = await route.fetch();
    const json = await response.json();
    json.member_mode = true;
    json.member_mode_no_credential = noCredential;
    json.role = "member";
    json.operator = false;
    json.security_operator = false;
    await route.fulfill({ response, json });
  });
}

/** Splices method:"sso" onto /me. The seeded backend authenticates with a bare
 *  admin BEARER token, so /me answers method:"token" — and the menu item is
 *  deliberately hidden for it, because the server refuses that lane a 400 (no
 *  per-person role to pause). This is the same splice, for the same reason
 *  mockMemberRole exists. */
async function mockSSOAdmin(page: import("@playwright/test").Page): Promise<void> {
  await page.route("**/api/v1/me", async (route) => {
    const response = await route.fetch();
    const json = await response.json();
    json.method = "sso";
    // The second entry is server-gated (0.7.5): /me only says "available" under
    // a per_user roster row, and the seeded e2e backend has no such row — so the
    // splice is what puts the console in the deployment shape this case is about.
    json.member_preview_available = true;
    await route.fulfill({ response, json });
  });
}

/** Captures POST /me/member-mode's body and answers 200 without touching the
 *  server (which would refuse this bearer-token caller a session it cannot
 *  clamp). Returns the array the bodies land in. */
async function captureToggle(page: import("@playwright/test").Page): Promise<unknown[]> {
  const bodies: unknown[] = [];
  await page.route("**/api/v1/me/member-mode", async (route) => {
    bodies.push(route.request().postDataJSON());
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ member_mode: true }),
    });
  });
  return bodies;
}

test.describe("member mode — the banner is the way out", () => {
  test("the banner states the mode, names the ceilings, and Exit posts enabled:false", async ({ page }) => {
    await mockMemberMode(page);
    const bodies = await captureToggle(page);
    await gotoConsole(page);

    const banner = page.getByText(MEMBER_MODE.BANNER);
    await expect(banner).toBeVisible();
    // The ceilings ride the banner itself, not a doc nobody opens mid-session.
    await expect(banner).toHaveAttribute("title", MEMBER_MODE.CEILINGS);

    // Every admin surface is gone while the mode is on — the same nav the
    // member-console spec pins, reached here through the mode instead of a
    // role splice.
    for (const label of ["Policies", "Secrets", "Audit"] as const) {
      await expect(sidebarLink(page, label)).toHaveCount(0);
    }

    await page.getByRole("button", { name: MEMBER_MODE.EXIT }).click();
    await expect.poll(() => bodies).toEqual([{ enabled: false }]);
  });

  test("the account menu offers 'View as member' to an admin, and posts enabled:true", async ({ page }) => {
    await mockSSOAdmin(page);
    const bodies = await captureToggle(page);
    await gotoConsole(page);

    // The account-menu trigger is the LAST button in the header (after "New
    // run" and "Toggle theme") — the same handle member-console.spec.ts uses.
    await page.locator("header").getByRole("button").last().click();
    const item = page.getByRole("menu").getByText(MEMBER_MODE.MENU);
    await expect(item).toBeVisible();
    await item.click();
    await expect.poll(() => bodies).toEqual([{ enabled: true }]);
  });

  // 0.7.5, field report finding 3. The preview is the posture that can show the
  // one state a per_user deployment's members are all in on day one, and the
  // console has to (a) offer it and (b) say what it does — the plain banner's
  // ceilings are false inside it.
  // Its own test rather than a second half of the one above, and that is not
  // tidiness: entering the mode navigates (onEntered's window.location.assign),
  // so a re-goto in the same test races that navigation and aborts it.
  test("the account menu offers BOTH postures, and the new one posts no_credential:true", async ({ page }) => {
    await mockSSOAdmin(page);
    const bodies = await captureToggle(page);
    await gotoConsole(page);

    await page.locator("header").getByRole("button").last().click();
    const menu = page.getByRole("menu");
    await expect(menu.getByText(MEMBER_MODE.MENU)).toBeVisible();
    const preview = menu.getByText(MEMBER_MODE.MENU_NEW);
    await expect(preview).toBeVisible();
    await preview.click();
    // The key rides ONLY the new posture: a 0.7.4 replica decodes this body
    // strictly, so the plain toggle above must keep sending {enabled} alone.
    await expect.poll(() => bodies).toEqual([{ enabled: true, no_credential: true }]);
  });

  test("the new-member preview paints its own banner and its own ceilings", async ({ page }) => {
    await mockMemberMode(page, true);
    await gotoConsole(page);

    const banner = page.getByText(MEMBER_MODE.BANNER_NEW);
    await expect(banner).toBeVisible();
    await expect(banner).toHaveAttribute("title", MEMBER_MODE.CEILINGS_NEW);
    // The plain sentence must be GONE, not merely joined: inside the preview its
    // ceiling 4 ("model access still resolves to you") is false.
    await expect(page.getByText(MEMBER_MODE.BANNER)).toHaveCount(0);
  });

  test("an admin NOT in member mode sees no banner", async ({ page }) => {
    await gotoConsole(page);
    await expect(page.getByText(MEMBER_MODE.BANNER)).toHaveCount(0);
  });
});
