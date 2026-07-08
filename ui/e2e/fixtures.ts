/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test as base, expect, type Locator, type Page } from "@playwright/test";

// Shared Playwright fixtures for the Wardyn UI e2e suite. Specs run against the
// seeded test backend booted by scripts/e2e-backend.sh (real wardynd + Postgres +
// `none` runner). See playwright.config.ts for the base URL.

// The admin token the seeded backend is started with. The app stores it under
// localStorage["wardyn_admin_token"] and probes /api/v1/runs on mount to decide
// auth; injecting it before first navigation boots the app already signed in.
export const ADMIN_TOKEN = process.env.WARDYN_E2E_TOKEN || "wardyn-e2e-token";
const TOKEN_KEY = "wardyn_admin_token";

// `test` boots the app pre-authenticated so each spec lands directly in the
// console. Auth-flow specs that exercise sign-in/sign-out should import the raw
// `test` from "@playwright/test" instead and manage storage themselves.
export const test = base.extend({
  page: async ({ page }, use) => {
    await page.addInitScript(
      ([key, tok]) => {
        try {
          localStorage.setItem(key, tok);
        } catch {
          /* private mode — ignore */
        }
      },
      [TOKEN_KEY, ADMIN_TOKEN]
    );
    await use(page);
  },
});

export { expect };

// Sidebar labels (app-shell.tsx). Navigating by accessible name keeps specs
// resilient to markup churn and needs no shared test-ids. Runs is the first/
// default screen; Fleet is retired from the nav (still routable at /fleet, but
// not a sidebar entry, so it is intentionally absent from this list).
export type NavLabel =
  | "Runs"
  | "Approvals"
  | "Policies"
  | "Secrets"
  | "Audit"
  | "Recordings"
  | "Getting started";

// Sidebar entries are react-router <NavLink>s (role="link"), not <button>s.
// Their accessible name can carry trailing content beyond the label — Runs/
// Approvals a numeric badge ("Runs 2"), Getting started a StatusChip word
// ("Getting started Ready" / "…Checking…") — so match by prefix rather than
// an exact/suffix pattern.
export function sidebarLink(page: Page, label: NavLabel): Locator {
  return page.getByRole("link", { name: new RegExp(`^${label}`) });
}

// gotoConsole loads the app shell (pre-authed) and waits for the sidebar.
export async function gotoConsole(page: Page): Promise<void> {
  await page.goto("/");
  await expect(sidebarLink(page, "Runs")).toBeVisible();
}

// navTo clicks a sidebar entry and returns once the click is registered.
export async function navTo(page: Page, label: NavLabel): Promise<void> {
  await sidebarLink(page, label).click();
}
