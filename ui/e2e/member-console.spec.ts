/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { randomUUID } from "node:crypto";
import { test, expect, ADMIN_TOKEN, gotoConsole, mockMemberRole, navToRoute, sidebarLink, sql } from "./fixtures";
import { DENIED } from "../src/app/lib/permissions-copy";

// This spec proves the RENDER behavior a member role drives (nav filtering,
// the chip, empty/count copy) on top of mockMemberRole's mocked-/me splice —
// see fixtures.ts for why the role is mocked and shared from there rather
// than declared per-spec (Playwright refuses a spec file that imports
// another spec file).

test.describe("member console — nav absence (mocked /me role)", () => {
  test("member nav is Runs · Approvals · Workspaces only — no admin-only items", async ({ page }) => {
    await mockMemberRole(page);
    await gotoConsole(page);

    // Workspaces joined the member set (mock M6): a member launches runs
    // against workspaces, and the New run picker was the only place they were
    // visible at all.
    for (const label of ["Runs", "Approvals", "Workspaces"] as const) {
      await expect(sidebarLink(page, label)).toBeVisible();
    }
    // MEMBER_NAV_PATHS (app-shell.tsx) is the single source of truth.
    // Permissions joined the sidebar in 0.6 and is admin-only: 0.6 ships no
    // member permissions screen at all, only the inline why-denied moments
    // below, so it must not appear here. Recordings is the admin evidence
    // trail and stays admin-only for the same reason Audit does.
    for (const label of ["Policies", "Governance", "Permissions", "Secrets", "Audit", "Recordings"] as const) {
      await expect(sidebarLink(page, label)).toHaveCount(0);
    }
  });

  test("the account menu shows a quiet 'user' chip", async ({ page }) => {
    await mockMemberRole(page);
    await gotoConsole(page);

    // The account-menu trigger is the LAST button in the header (after "New
    // run" and "Toggle theme" — see app-shell.tsx's TopBar); its Radix
    // DropdownMenuContent renders role="menu" once opened.
    await page.locator("header").getByRole("button").last().click();
    const menu = page.getByRole("menu");
    await expect(menu.getByText("user", { exact: true })).toBeVisible();
  });

  test("admin (unmocked, today's default): the full nine-item nav", async ({ page }) => {
    await gotoConsole(page);
    for (const label of ["Runs", "Approvals", "Workspaces", "Policies", "Governance", "Permissions", "Secrets", "Audit", "Recordings"] as const) {
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


// ---------------------------------------------------------------------------
// 0.6 pillar 2 — the member why-denied moments. Same mocked-/me technique as
// above: the seeded backend authenticates with a bare admin bearer, so the
// SERVER always answers as an admin; what these tests prove is the RENDER a
// member role drives on top of a real, enforced capability state (the
// enforcement switches below are written through the real PUT, and
// GET /me/capabilities answers from the real tables).
//
// A bearer caller has no OIDC human identity, so /me/capabilities honestly
// reports zero grants and a NIL group snapshot — which is exactly the
// ungranted, stale-session member this surface is written for.
test.describe.configure({ mode: "serial" });

test.describe("member why-denied (mocked /me role, real enforcement)", () => {
  test.beforeEach(async ({ page }) => {
    await page.request.put("/api/v1/permissions/enforcement", {
      headers: { Authorization: `Bearer ${ADMIN_TOKEN}` },
      data: { egress_host: true, workspace: true },
    });
  });

  test("an ungranted egress host: both decisions disable, with the reason and the Always caveat", async ({ page }) => {
    const runId = sql("SELECT id FROM agent_runs ORDER BY created_at LIMIT 1");
    expect(runId, "no seeded runs found — is the backend up and seeded?").not.toBe("");
    sql(`DELETE FROM approvals WHERE state = 'PENDING'`);
    sql(
      `INSERT INTO approvals (id, run_id, kind, requested_scope, state, requested_at)
       VALUES ('${randomUUID()}', '${runId}', 'egress_domain', '{"host":"registry.npmjs.org"}'::jsonb, 'PENDING', now())`,
    );

    await mockMemberRole(page);
    await gotoConsole(page);
    await navToRoute(page, "/approvals");

    await expect(page.getByRole("button", { name: /^Approve$/ })).toBeDisabled();
    await expect(page.getByRole("button", { name: /^Deny$/ })).toBeDisabled();
    await expect(page.getByText(DENIED.APPROVE_CHIP)).toBeVisible();
    await expect(page.getByText(DENIED.APPROVE_BODY("registry.npmjs.org"))).toBeVisible();
    await expect(page.getByText(DENIED.ALWAYS_STILL_ADMIN)).toBeVisible();
    // A bearer session carries no recorded groups, so the snapshot ceiling is
    // named too — the member's half of the stale-groups story.
    await expect(page.getByText(DENIED.STALE_GROUPS)).toBeVisible();
  });

  test("an ungranted workspace stays LISTED and annotated — visibility is not capability", async ({ page }) => {
    await mockMemberRole(page);
    await gotoConsole(page);
    await navToRoute(page, "/runs/new");
    await expect(page.getByRole("heading", { name: "New run" })).toBeVisible();

    // The workspace Select's trigger carries no accessible name of its own —
    // its content IS the current selection, so filter on that.
    const picker = page.getByRole("combobox").filter({ hasText: "Ephemeral scratch" });
    await picker.click();
    const option = page.getByRole("option", { name: /payments/ });
    await expect(option).toBeVisible();
    await expect(option.getByText(DENIED.WORKSPACE_CHIP)).toBeVisible();

    await option.click();
    await expect(page.getByText(DENIED.WORKSPACE_BODY)).toBeVisible();
  });
});

// ---------------------------------------------------------------------------
// X3-F1 / X3-F4 — the console used to read a member's REDACTED body as facts
// about the deployment. The harness backend always answers as an admin (see
// fixtures.ts), so the member-shaped body is spliced the same way the role is:
// redactSetupStatusForUser zeroes the runner struct (Driver "" — the Go zero
// value, not the "none" sentinel), empties checks, and now says so with
// checks_redacted.
async function mockMemberSetupStatus(page: import("@playwright/test").Page): Promise<void> {
  await page.route("**/api/v1/setup/status", async (route) => {
    const response = await route.fetch();
    const json = await response.json();
    json.checks = [];
    json.providers = [];
    json.secrets = { present: [] };
    json.checks_redacted = true;
    // The point of the shape: the driver NAME is withheld while the classes
    // survive the redaction. The harness backend may itself report none, so
    // the fixture supplies a live pair when it does.
    const classes: string[] = json.runner?.confinement_classes ?? [];
    json.runner = { driver: "", confinement_classes: classes.length > 0 ? classes : ["CC1", "CC2"] };
    await route.fulfill({ response, json });
  });
}

test.describe("member console — a redacted body is not a deployment fact", () => {
  test("Settings claims neither a missing runner nor an off image builder", async ({ page }) => {
    await mockMemberRole(page);
    await mockMemberSetupStatus(page);
    await gotoConsole(page);
    await navToRoute(page, "/settings");

    await expect(page.getByRole("heading", { name: "Host", level: 3 })).toBeVisible();
    // The barrier picker is the member's own choice and stays live — the card
    // and its `-runner docker` fix are for a host that genuinely has none.
    await expect(page.getByText("No sandbox runner")).toHaveCount(0);
    await expect(page.getByText("-runner docker")).toHaveCount(0);
    await expect(page.getByRole("radio", { name: /Wall/ })).toBeEnabled();
    // The row itself is gone, not merely its Off sentence — the deployment's
    // builder posture was never told to this caller.
    await expect(page.getByText("Image builder")).toHaveCount(0);
    await expect(page.getByText(/devcontainer builds and --image wraps are unavailable/)).toHaveCount(0);
  });

  test("an empty board offers the member's own next move, not the operator funnel", async ({ page }) => {
    await mockMemberRole(page);
    await page.route(/\/api\/v1\/runs(\?|$)/, (route) =>
      route.request().method() === "GET" ? route.fulfill({ json: [] }) : route.continue(),
    );
    await gotoConsole(page);
    await navToRoute(page, "/runs");

    await expect(page.getByText("Runs you launch appear here")).toBeVisible();
    await expect(page.getByText("No runs yet")).toHaveCount(0);
    await expect(page.getByRole("link", { name: "New run" })).toHaveAttribute("href", "/runs/new");
    await expect(page.getByRole("link", { name: "Getting started" })).toHaveAttribute("href", "/setup");
  });
});
