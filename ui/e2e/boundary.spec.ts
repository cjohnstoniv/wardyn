/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Policies -> Wardyn boundary against the real server: a security admin reaches the page from Policies, a fresh
// install's boundary is seeded in hard_limits (#3238), so the page opens on the Hard limits mode with its list,
// and nothing is offered for review until something changes. No second-admin claim is made before a submit. The
// Desktop group card (M7-Knobs-B, #2967) shows the value each direction is held to and drafts into the dry run.
import { test, expect, asRealSecurityAdmin, consoleAPI, gotoConsole, navTo } from "./fixtures";
import { BOUNDARY, BOUNDARY_DESKTOP, BOUNDARY_DRY_RUN } from "../src/app/lib/boundary-copy";

// Serial: the second test moves the install's one boundary document and puts it back.
test.describe.configure({ mode: "serial" });

test.describe("Policies: the Wardyn boundary", () => {
  test("a fresh install reads Hard limits, with the list shown and nothing to review", async ({ page }) => {
    await asRealSecurityAdmin(page);
    await gotoConsole(page, "admin");
    await navTo(page, "Policies");
    // The link sits in the page body, not the header, so the header keeps its one action (#2831).
    await expect(page.getByTestId("page-header-actions").getByRole("link", { name: BOUNDARY.TITLE })).toHaveCount(0);
    await page.getByRole("link", { name: BOUNDARY.TITLE, exact: true }).click();
    await expect(page).toHaveURL(/\/admin\/policies\/boundary$/);
    await expect(page.getByRole("heading", { level: 1, name: BOUNDARY.TITLE })).toBeVisible();
    await expect(page.getByTestId("boundary-mode-chip")).toHaveText(BOUNDARY.CHIP_HARD);

    const hard = page.getByRole("radio", { name: new RegExp(BOUNDARY.MODE_HARD_BODY.slice(0, 20)) });
    await expect(hard).toBeChecked();
    await expect(hard).toBeEnabled();
    await expect(page.getByTestId("boundary-hard-limits")).toBeVisible();
    await expect(page.getByText(BOUNDARY.HARD_HEADING)).toBeVisible();
    await expect(page.getByTestId("boundary-org")).toHaveCount(0);
    await expect(page.getByRole("button", { name: BOUNDARY.REVIEW, exact: true })).toBeDisabled();
    await expect(page.getByText(/second admin|another security admin/)).toHaveCount(0);
  });

  test("an install whose boundary is no_limits can choose Hard limits, and Review shows the dry run before any save", async ({ page }) => {
    await asRealSecurityAdmin(page);
    const path = "/api/v1/governance/boundary";
    const original = await consoleAPI(page, "GET", path);
    expect(original.status, original.text).toBe(200);
    const { mode, spec, limits } = JSON.parse(original.text);
    const off = await consoleAPI(page, "PUT", path, { mode: "no_limits", spec: {}, limits: {} });
    expect(off.status, off.text).toBe(200);
    try {
      await gotoConsole(page, "admin");
      await navTo(page, "Policies");
      await page.getByRole("link", { name: BOUNDARY.TITLE, exact: true }).click();
      await expect(page.getByTestId("boundary-mode-chip")).toHaveText(BOUNDARY.MODE_ORG);

      const hard = page.getByRole("radio", { name: new RegExp(BOUNDARY.MODE_HARD_BODY.slice(0, 20)) });
      await expect(hard).toBeEnabled();
      await hard.check();
      await expect(page.getByTestId("boundary-hard-limits")).toBeVisible();
      await page.getByRole("button", { name: BOUNDARY.REVIEW, exact: true }).click();
      await expect(page.getByTestId("boundary-dry-run")).toBeVisible();
      // Nothing is written until Submit: the stored document is still no_limits.
      const stored = await consoleAPI(page, "GET", path);
      expect(JSON.parse(stored.text).mode).toBe("no_limits");
    } finally {
      const back = await consoleAPI(page, "PUT", path, { mode, spec, limits });
      expect(back.status, back.text).toBe(200);
    }
  });

  test("the dry run names an assignment's kind, so an everyone row is not a bare ' · team'", async ({ page }) => {
    await asRealSecurityAdmin(page);
    const change = [{ knob: "limits.default_end_sec", before: 3600, after: 1800 }];
    await page.route("**/api/v1/governance/dry-run", (route) =>
      route.fulfill({
        json: {
          deployment: [],
          units: [],
          stored_policies: [],
          memberships: [{ id: "a1", subject_type: "all", subject: "", unit_id: "p1", unit_name: "Platform team", changes: change }],
        },
      }),
    );
    const path = "/api/v1/governance/boundary";
    const original = await consoleAPI(page, "GET", path);
    const { mode, spec, limits } = JSON.parse(original.text);
    const off = await consoleAPI(page, "PUT", path, { mode: "no_limits", spec: {}, limits: {} });
    expect(off.status, off.text).toBe(200);
    try {
      await gotoConsole(page, "admin");
      await navTo(page, "Policies");
      await page.getByRole("link", { name: BOUNDARY.TITLE, exact: true }).click();
      await page.getByRole("radio", { name: new RegExp(BOUNDARY.MODE_HARD_BODY.slice(0, 20)) }).check();
      await page.getByRole("button", { name: BOUNDARY.REVIEW, exact: true }).click();
      const report = page.getByTestId("boundary-dry-run");
      await expect(report.getByText(BOUNDARY_DRY_RUN.OTHER_ASSIGNMENTS)).toBeVisible();
      await expect(report.getByText(BOUNDARY_DRY_RUN.ASSIGNMENT_ROW("all", "", "Platform team"))).toBeVisible();
    } finally {
      const back = await consoleAPI(page, "PUT", path, { mode, spec, limits });
      expect(back.status, back.text).toBe(200);
    }
  });

  test("the Desktop card holds an unset direction to its default and drafts a change into the dry run", async ({ page }) => {
    await asRealSecurityAdmin(page);
    const path = "/api/v1/governance/boundary";
    const original = await consoleAPI(page, "GET", path);
    expect(original.status, original.text).toBe(200);
    const { mode, spec, limits } = JSON.parse(original.text);
    const set = await consoleAPI(page, "PUT", path, { mode: "hard_limits", spec: { ...spec, desktop: { copy_out: "allowed" } }, limits });
    expect(set.status, set.text).toBe(200);
    try {
      await gotoConsole(page, "admin");
      await navTo(page, "Policies");
      await page.getByRole("link", { name: BOUNDARY.TITLE, exact: true }).click();
      const card = page.getByTestId("boundary-desktop");
      await expect(card).toBeVisible();
      const row = (name: string) => card.getByRole("group", { name });
      await expect(row("Copy out of the desktop").getByRole("radio", { name: BOUNDARY_DESKTOP.ALLOWED })).toBeChecked();
      await expect(row("Download files").getByRole("radio", { name: BOUNDARY_DESKTOP.BLOCKED })).toBeChecked();
      await expect(page.getByTestId("boundary-desktop-now")).toContainText("Download blocked");

      await row("Copy out of the desktop").getByText(BOUNDARY_DESKTOP.BLOCKED, { exact: true }).click();
      await page.getByRole("button", { name: BOUNDARY.REVIEW, exact: true }).click();
      const report = page.getByTestId("boundary-dry-run");
      await expect(report).toBeVisible();
      await expect(report.getByRole("row", { name: new RegExp(`^${BOUNDARY_DESKTOP.TITLE}`) })).toContainText("Copy out blocked");
      const stored = await consoleAPI(page, "GET", path);
      expect(JSON.parse(stored.text).spec.desktop).toEqual({ copy_out: "allowed" });
    } finally {
      const back = await consoleAPI(page, "PUT", path, { mode, spec, limits });
      expect(back.status, back.text).toBe(200);
    }
  });
});
