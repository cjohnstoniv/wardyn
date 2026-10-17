/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, asRealMember, asRealSecurityAdmin, consoleAPI, expandCard, gotoConsole, navToRoute } from "./fixtures";

// 0.8.6 scim-a7 (mock packet M5): the Settings SCIM card and the read-only GET /api/v1/scim/status
// behind it. The e2e backend has no SCIM token, so the real answer is "not set up"; the set-up states
// are a hypothetical body spliced into that one GET, the render-only precedent of the member-role mocks.
// The unit suite (scim-card.test.tsx) pins every string; this proves the card is mounted on the real
// Settings screen, reads the real route, and that the route's tier is the security admin's, not a member's.

const card = (page: import("@playwright/test").Page) =>
  page.locator("section").filter({ has: page.getByRole("heading", { name: "SCIM provisioning" }) });

test.describe("Admin Settings, SCIM provisioning", () => {
  test("SCIM off: the card says so, names the fix and links the runbook", async ({ page }) => {
    await gotoConsole(page, "admin");
    await navToRoute(page, "/admin/settings");
    await expect(card(page)).toBeVisible();
    await expect(card(page).getByRole("button", { name: /^SCIM provisioning\s*Not set up/ })).toBeVisible();
    await expandCard(page, "SCIM provisioning");
    await expect(
      card(page).getByText("Not set up. Someone removed in your identity provider keeps their sessions, tokens and runs here until an admin revokes them."),
    ).toBeVisible();
    await expect(card(page).getByText(/^Set WARDYN_SCIM_TOKEN on wardynd, then point your Entra ID provisioning at the endpoint below\./)).toBeVisible();
    await expect(card(page).getByRole("link", { name: "Leaver runbook" })).toHaveAttribute("href", /OPERATIONS\.md#leavers-and-scim$/);
    await expect(card(page).getByText(/\/scim\/v2$/)).toBeVisible();
  });

  test("SCIM on: facts, deactivated people, unfinished steps and drives to reclaim", async ({ page }) => {
    await gotoConsole(page, "admin");
    const day = 86_400_000;
    await page.route("**/api/v1/scim/status", (route) =>
      route.fulfill({
        json: {
          configured: true,
          last_token_slot: "next",
          purge_after_seconds: 30 * 86400,
          keep_workspaces: false,
          deactivated: [
            { person: "ada@example.com", deactivated_at: new Date(Date.now() - 2 * day).toISOString(), purge_after: new Date(Date.now() + 28 * day).toISOString() },
            { person: "bob@example.com", deactivated_at: new Date(Date.now() - day).toISOString() },
          ],
          pending: [{ person: "ada@example.com", step: "kill_run", last_error: "runner: teardown timed out" }],
          drives: [{ person: "bob@example.com", drive: "Team share", purged_at: new Date(Date.now() - day).toISOString() }],
        },
      }),
    );
    await navToRoute(page, "/admin/settings");
    await expect(card(page).getByRole("button", { name: /^SCIM provisioning\s*2 deactivated · 1 unfinished/ })).toBeVisible();
    await expandCard(page, "SCIM provisioning");
    await expect(card(page).getByText("Next token", { exact: true })).toBeVisible();
    await expect(card(page).getByText(/^Your identity provider is using the next token\./)).toBeVisible();
    await expect(card(page).getByText("30 days", { exact: true })).toBeVisible();
    await expect(card(page).getByText("Handed to the operator")).toBeVisible();
    await expect(card(page).getByText("ada@example.com").first()).toBeVisible();
    await expect(card(page).getByText("Not scheduled", { exact: true })).toBeVisible();
    await expect(card(page).getByText("Stop runs", { exact: true })).toBeVisible();
    await expect(card(page).getByText("runner: teardown timed out")).toBeVisible();
    await expect(card(page).getByText("Team share", { exact: true })).toBeVisible();
    await expect(card(page).getByText(/^A purge lists these and leaves them in place\./)).toBeVisible();
  });
});

test.describe("GET /scim/status tiers", () => {
  test("a security admin may read it; a member may not", async ({ page }) => {
    await asRealSecurityAdmin(page);
    const read = await consoleAPI(page, "GET", "/api/v1/scim/status");
    expect(read.status, read.text).toBe(200);
    expect(JSON.parse(read.text)).toMatchObject({ configured: false, deactivated: [], pending: [], drives: [] });

    await asRealMember(page);
    const refused = await consoleAPI(page, "GET", "/api/v1/scim/status");
    expect(refused.status, refused.text).toBe(403);
  });
});
