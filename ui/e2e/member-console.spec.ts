/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, gotoConsole, sidebarLink } from "./fixtures";

// Member console (B3) — the seeded e2e backend authenticates every spec with
// a bare admin bearer token (fixtures.ts's ADMIN_TOKEN), and isOperator
// (internal/api/http.go) reads "no session role to demote" for any caller
// with no OIDC human session — so a bearer-token caller is ALWAYS admin
// server-side; there is no way to reach a genuine member session through
// this harness without standing up OIDC. Per the lane brief's own fallback
// ("Playwright spec on a MOCKED /me … no live backend"), GET /api/v1/me's
// `role`/`operator` fields are spliced onto the REAL response (route.fetch()
// + patch + refulfill — same technique corp-network.spec.ts already uses)
// so principal/method stay genuine while the client believes it is signed in
// as a member. Everything else (runs list, approvals list) still comes from
// the real, unmodified, admin-scoped backend — this spec proves the RENDER
// behavior a member role drives (nav filtering, the chip, empty/count copy),
// not server-side ownership scoping itself (that's proven server-side: B2's
// own tests, and internal/api/runs_policy.go's handleListRuns /
// approvals.go's handleListApprovals creator-pager branches).
async function mockMemberRole(page: import("@playwright/test").Page): Promise<void> {
  await page.route("**/api/v1/me", async (route) => {
    const response = await route.fetch();
    const json = await response.json();
    json.role = "member";
    json.operator = false;
    await route.fulfill({ response, json });
  });
}

test.describe("member console — nav absence (mocked /me role)", () => {
  test("member nav is Runs · Approvals only — no admin-only items", async ({ page }) => {
    await mockMemberRole(page);
    await gotoConsole(page);

    for (const label of ["Runs", "Approvals"] as const) {
      await expect(sidebarLink(page, label)).toBeVisible();
    }
    // Workspaces is asserted absent too: the flat nav made it a first-class
    // sidebar entry, so a member must not see it. MEMBER_NAV_PATHS
    // (app-shell.tsx) is the single source of truth — Runs and Approvals only.
    for (const label of ["Workspaces", "Policies", "Secrets", "Audit"] as const) {
      await expect(sidebarLink(page, label)).toHaveCount(0);
    }
  });

  test("the account menu shows a quiet 'member' chip", async ({ page }) => {
    await mockMemberRole(page);
    await gotoConsole(page);

    // The account-menu trigger is the LAST button in the header (after "New
    // run" and "Toggle theme" — see app-shell.tsx's TopBar); its Radix
    // DropdownMenuContent renders role="menu" once opened.
    await page.locator("header").getByRole("button").last().click();
    const menu = page.getByRole("menu");
    await expect(menu.getByText("member", { exact: true })).toBeVisible();
  });

  test("admin (unmocked, today's default): the full six-item nav", async ({ page }) => {
    await gotoConsole(page);
    for (const label of ["Runs", "Approvals", "Workspaces", "Policies", "Secrets", "Audit"] as const) {
      await expect(sidebarLink(page, label)).toBeVisible();
    }
  });
});

test.describe("foreign/unknown run — standard not-found (absence, not refusal)", () => {
  test("a well-formed but nonexistent run id renders the SAME generic not-found state a bogus one would", async ({ page }) => {
    // No role mock needed: getRunAuthorized's byte-identical 404 (foreign vs
    // genuinely missing) means this state renders identically for admin and
    // member — this spec proves the CLIENT never special-cases it either way.
    await gotoConsole(page);
    await page.goto("/runs/00000000-0000-0000-0000-000000000000");

    await expect(page.getByText("Run not found")).toBeVisible();
    // run-detail.tsx now also names "you don't have access to it" alongside
    // archived/deleted/stale-link (run-detail.test.tsx: "names lack-of-access
    // as a real reason") — undifferentiated from the other reasons, so the
    // anti-enumeration property this spec is about still holds.
    await expect(
      page.getByText(/this run may have been archived or deleted, the link is stale, or you don't have access to it/i),
    ).toBeVisible();
    // No denial-flavored copy — the product must never confirm the id was
    // ever real (no existence oracle).
    await expect(page.getByText(/denied|forbidden|no permission|not authorized/i)).toHaveCount(0);
    await expect(page.getByRole("button", { name: /back to runs/i })).toBeVisible();
  });
});
