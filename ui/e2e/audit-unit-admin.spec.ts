/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { Page } from "@playwright/test";
import { test, expect, asRealMember } from "./fixtures";

// The Audit page for a unit admin: a person who administers a unit and is not a security admin. The server's
// unit-scoped audit read is pinned in Go; this spec walks what the console shows from /me's `unit_admin_of` and
// the rows the server serves, both answered at the network boundary.

const UNITS = [{ unit_id: "u-e2e", unit_name: "e2e-unit" }];

const row = (id: string, runId: string, action: string, target: string, outcome: string) => ({
  id,
  time: new Date().toISOString(),
  actor_type: "agent",
  actor: "agent",
  action,
  target,
  outcome,
  run_id: runId,
});

/**
 * The member session /me answers as, with the units it administers. The page is mounted under /admin, a path the
 * console's view gate lets a plain-user role through only when its /me names an admin tier, so the role is spliced
 * with the units; operator and security_operator stay false, which is what the page's gate reads.
 */
async function asUnitAdmin(page: Page, units: unknown[] = UNITS): Promise<void> {
  await asRealMember(page);
  let base: Record<string, unknown> | null = null;
  await page.route("**/api/v1/me", async (route) => {
    base ??= (await (await route.fetch()).json()) as Record<string, unknown>;
    await route.fulfill({ json: { ...base, role: "admin", operator: false, security_operator: false, unit_admin_of: units } });
  });
}

async function serveAudit(page: Page, rows: unknown[]): Promise<void> {
  await page.route(/\/api\/v1\/audit(\?|$)/, (route) => route.fulfill({ json: rows }));
}

const DESCRIPTION =
  "Append-only. Every egress decision, brokered credential, approval and enforcement action, across the runs in your units.";

test("a unit admin sees the unit description, a note naming their unit, and the rows", async ({ page }) => {
  await asUnitAdmin(page);
  await serveAudit(page, [
    row("e-unit-1", "run_e2eunit0001", "egress.allow", "api.github.com:443", "success"),
    row("e-unit-2", "run_e2eunit0002", "egress.deny", "pastebin.example:443", "denied"),
  ]);
  await page.goto("/admin/audit");

  await expect(page.getByText(DESCRIPTION)).toBeVisible();
  await expect(page.getByText("Runs in your units", { exact: true })).toBeVisible();
  await expect(page.getByText("e2e-unit", { exact: true })).toBeVisible();
  await expect(page.getByText("Allowed egress to api.github.com:443")).toBeVisible();
  await expect(page.getByText("Denied egress to pastebin.example:443")).toBeVisible();
  await expect(page.getByText(/^2 events$/)).toBeVisible();
});

test("a unit admin with no rows yet is told there are no runs in their units", async ({ page }) => {
  await asUnitAdmin(page);
  await serveAudit(page, []);
  await page.goto("/admin/audit");

  await expect(page.getByText("No runs in your units yet.")).toBeVisible();
  await expect(page.getByText("The full audit feed is admin-only.")).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Open Runs" })).toBeVisible();
});

test("a run reference opens the audit drill, whose banner has no Open run link, and Back clears it", async ({ page }) => {
  await asUnitAdmin(page);
  const runId = "run_e2eunit0001";
  await page.route(/\/api\/v1\/audit(\?|$)/, (route) => route.fulfill({ json: [row("e-unit-1", runId, "egress.allow", "api.github.com:443", "success")] }));
  // A unit admin is refused the run page of a run they do not own: the same 404 as a run that is not there.
  await page.route(`**/api/v1/runs/${runId}`, (route) => route.fulfill({ status: 404, json: { error: "run not found" } }));
  await page.goto("/admin/audit");

  await page.locator('main [data-testid="run-ref"]').first().getByRole("button").first().click();
  await expect(page).toHaveURL(new RegExp(`run_id=${runId}`));
  await expect(page.getByText("This run is in your unit")).toBeVisible();
  await expect(page.getByRole("link", { name: /Open run/ })).toHaveCount(0);

  await page.getByRole("button", { name: "Back to all runs in your units" }).click();
  await expect(page.getByText("This run is in your unit")).toHaveCount(0);
  await expect(page).not.toHaveURL(/run_id=/);
});

test("a member who administers no unit still reads that the full feed is admin-only", async ({ page }) => {
  await asUnitAdmin(page, []);
  await serveAudit(page, []);
  await page.goto("/admin/audit");

  await expect(page.getByText("The full audit feed is admin-only.")).toBeVisible();
  await expect(page.getByText("Runs in your units")).toHaveCount(0);
});
