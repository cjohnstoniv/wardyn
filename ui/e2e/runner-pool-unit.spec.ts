/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { randomUUID } from "node:crypto";
import { test, expect, ADMIN_TOKEN, asRealSecurityAdmin, consoleAPI, gotoConsole } from "./fixtures";
import { POOL_DRAWER } from "../src/app/components/wardyn/copy/runner-pools";
import { POOL_UNIT } from "../src/app/components/wardyn/copy/pool-unit";

// "Who may use it" naming a unit, against the real server: an organization pool's policy names a profile, the
// saved row reads the profile's name, and once the profile is deleted the row says it admits no one.
const auth = { Authorization: `Bearer ${ADMIN_TOKEN}` };
const suffix = randomUUID().slice(0, 8);
const UNIT_NAME = `pool-unit-${suffix}`;
const POOL_NAME = `unit-pool-${suffix}`;
const CEILING = {
  allowed_domains: ["api.anthropic.com"],
  first_use_approval: "deny_with_review",
  min_confinement_class: "CC3",
  eligible_grants: [],
  tool_rules: [{ tool: "*", effect: "allow" }],
};

test.describe("runner pools — a policy names a unit", () => {
  test.describe.configure({ mode: "serial" });
  let unitId = "";
  let poolId = "";

  test.beforeAll(async ({ request }) => {
    const made = await request.post("/api/v1/governance/units", { headers: auth, data: { name: UNIT_NAME, ceiling: CEILING, limits: {} } });
    expect(made.status(), await made.text()).toBe(201);
    unitId = (await made.json()).unit.id as string;
    const pool = await request.post("/api/v1/runner-pools", { headers: auth, data: { name: POOL_NAME } });
    expect(pool.status(), await pool.text()).toBe(201);
    poolId = (await pool.json()).id as string;
  });

  test.afterAll(async ({ request }) => {
    if (poolId) await request.delete(`/api/v1/runner-pools/${poolId}`, { headers: auth });
    if (unitId) await request.delete(`/api/v1/governance/units/${unitId}`, { headers: auth });
  });

  test("a pool policy names a unit by profile", async ({ page }) => {
    await asRealSecurityAdmin(page);
    await gotoConsole(page, "admin");
    await page.goto(`/admin/runners?tab=pools&pool=${poolId}`);
    const sheet = page.getByRole("dialog");
    await sheet.getByRole("button", { name: POOL_DRAWER.EDIT_WHO }).click();
    await sheet.getByRole("combobox", { name: POOL_DRAWER.ADD }).click();
    await page.getByRole("option", { name: POOL_DRAWER.SUBJECT.unit, exact: true }).click();
    await expect(sheet.getByText(POOL_UNIT.HELP)).toBeVisible();
    await sheet.getByRole("combobox", { name: POOL_UNIT.LABEL }).click();
    await page.getByRole("option", { name: UNIT_NAME, exact: true }).click();
    await sheet.getByRole("button", { name: POOL_DRAWER.ADD_TO_LIST }).click();
    await sheet.getByRole("button", { name: POOL_DRAWER.SAVE_WHO }).click();
    const saved = sheet.getByRole("listitem").filter({ hasText: UNIT_NAME });
    await expect(saved).toContainText(POOL_DRAWER.SUBJECT.unit);
    await expect(saved).toContainText(POOL_UNIT.BELOW);

    const read = await consoleAPI(page, "GET", `/api/v1/runner-pools/${poolId}/use-policy`);
    expect(read.status, read.text).toBe(200);
    expect((JSON.parse(read.text) as { subjects: { subject_type: string; subject: string }[] }).subjects).toContainEqual({ subject_type: "unit", subject: unitId });
  });

  test("a deleted profile's row says it admits no one", async ({ page, request }) => {
    const gone = await request.delete(`/api/v1/governance/units/${unitId}`, { headers: auth });
    expect(gone.status(), await gone.text()).toBe(204);
    unitId = "";
    await asRealSecurityAdmin(page);
    await gotoConsole(page, "admin");
    await page.goto(`/admin/runners?tab=pools&pool=${poolId}`);
    const sheet = page.getByRole("dialog");
    await expect(sheet.getByText(POOL_UNIT.ORPHAN)).toBeVisible();
  });
});
