/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { Page } from "@playwright/test";
import { test, expect, mockMemberRole } from "./fixtures";
import {
  VIEW_ADMIN_TOKEN,
  VIEW_REFUSAL,
  VIEW_TO_ADMIN,
  VIEW_TO_USER,
} from "../src/app/components/wardyn/copy/console-view";

// M-1a — view routing: the Admin view is /admin/*, the User view every other
// path (admin-member-modes-design.md §2.3).
//
// The seeded backend authenticates with a bare admin bearer and no identity
// provider, which is a single-operator install: both views, the URL decides.
// Every other principal is a /me (or /healthz) splice — the same route.fetch +
// patch idiom fixtures.ts#mockMemberRole uses. They prove what the CONSOLE
// does for each principal; the server's own refusals are pinned in Go.

async function patchJSON(page: Page, glob: string, patch: (json: Record<string, unknown>) => void): Promise<void> {
  await page.route(glob, async (route) => {
    const response = await route.fetch();
    const json = await response.json();
    patch(json);
    await route.fulfill({ response, json });
  });
}

/** An SSO admin, in the Admin view (a fresh session carries no clamp). */
const ssoAdmin = (page: Page) => patchJSON(page, "**/api/v1/me", (j) => { j.method = "sso"; });

/** An SSO admin in the User view: the server answers them as a user, and says so. */
const ssoAdminInUserView = (page: Page) =>
  patchJSON(page, "**/api/v1/me", (j) => {
    Object.assign(j, { method: "sso", member_mode: true, role: "member", operator: false, security_operator: false });
  });

function auditRequests(page: Page): string[] {
  const seen: string[] = [];
  page.on("request", (r) => {
    if (new URL(r.url()).pathname === "/api/v1/audit") seen.push(r.url());
  });
  return seen;
}

test.describe("view routing", () => {
  test("a user who opens an Admin-view page is refused, and nothing behind it is fetched", async ({ page }) => {
    await mockMemberRole(page);
    const audit = auditRequests(page);
    await page.goto("/admin/audit");

    await expect(page.getByRole("heading", { name: VIEW_REFUSAL.TITLE })).toBeVisible();
    await expect(page.getByText(VIEW_REFUSAL.BODY)).toBeVisible();
    expect(audit).toEqual([]);

    await page.getByRole("button", { name: VIEW_REFUSAL.CTA }).click();
    await expect(page).toHaveURL(/\/runs$/);
  });

  test("an admin in the User view is asked before entering the Admin view, never redirected", async ({ page }) => {
    await ssoAdminInUserView(page);
    const audit = auditRequests(page);
    const bodies: unknown[] = [];
    await page.route("**/api/v1/me/member-mode", async (route) => {
      bodies.push(route.request().postDataJSON());
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ member_mode: false }) });
    });
    await page.goto("/admin/audit");

    await expect(page.getByRole("heading", { name: VIEW_TO_ADMIN.TITLE })).toBeVisible();
    await expect(page).toHaveURL(/\/admin\/audit$/);
    expect(audit).toEqual([]);

    await page.getByRole("button", { name: VIEW_TO_ADMIN.GO }).click();
    await expect.poll(() => bodies).toEqual([{ enabled: false }]);
  });

  test("staying in the User view goes to your runs", async ({ page }) => {
    await ssoAdminInUserView(page);
    await page.goto("/admin/policies");
    await page.getByRole("button", { name: VIEW_TO_ADMIN.STAY }).click();
    await expect(page).toHaveURL(/\/runs$/);
  });

  test("an admin in the Admin view who opens /runs/:id lands on its Admin-view twin", async ({ page }) => {
    await ssoAdmin(page);
    const id = "00000000-0000-4000-8000-000000000000";
    await page.goto(`/runs/${id}`);
    await expect(page).toHaveURL(new RegExp(`/admin/runs/${id}$`));
  });

  test("an admin in the Admin view is asked before starting a run", async ({ page }) => {
    await ssoAdmin(page);
    await page.goto("/runs/new");
    await expect(page.getByRole("heading", { name: VIEW_TO_USER.TITLE })).toBeVisible();
    await expect(page.getByText(VIEW_TO_USER.BODY)).toBeVisible();
    await page.getByRole("button", { name: VIEW_TO_USER.STAY }).click();
    await expect(page).toHaveURL(/\/admin\/runs$/);
  });

  test("the admin token on an SSO install is told User-view pages belong to a person", async ({ page }) => {
    await patchJSON(page, "**/healthz", (j) => { j.sso = true; });
    await page.goto("/runs");
    await expect(page.getByText(VIEW_ADMIN_TOKEN.BODY)).toBeVisible();
    await page.getByRole("button", { name: VIEW_ADMIN_TOKEN.CTA }).click();
    await expect(page).toHaveURL(/\/admin\/(runs|setup)/);
  });

  test("a single-operator install opens both views by URL", async ({ page }) => {
    await page.goto("/admin/audit");
    await expect(page.getByRole("heading", { name: "Audit", level: 1 })).toBeVisible();
    await page.goto("/runs");
    await expect(page).toHaveURL(/\/runs$/);
    await expect(page.getByRole("heading", { name: VIEW_TO_USER.TITLE })).toHaveCount(0);
  });

  test("D1: a single-operator install lands in the Admin view's setup until onboarded, then in the User view", async ({ page }) => {
    let onboarded = false;
    await patchJSON(page, "**/api/v1/setup/status*", (j) => {
      Object.assign(j, { has_runs: false, onboarding_complete: onboarded });
    });
    await page.goto("/");
    await expect(page).toHaveURL(/\/admin\/setup/);

    onboarded = true;
    await page.goto("/");
    await expect(page).toHaveURL(/\/runs$/);
  });
});
