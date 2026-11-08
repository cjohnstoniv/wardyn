/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import {
  test,
  expect,
  ADMIN_TOKEN,
  asRealMember,
  asRealUnitAdmin,
  consoleAPI,
  gotoConsole,
  grantUnitAdmin,
  navToRoute,
  sidebarLink,
} from "./fixtures";
import { CHANGES, GOVERNANCE as GOV } from "../src/app/lib/governance-copy";
import { VIEW_REFUSAL } from "../src/app/components/wardyn/copy/console-view";

// A real role `user` who holds a unit-admin grant reaches Governance and Audit; the same person
// without a grant is refused. Nothing here is spliced: the grant is written with the admin token and
// /me, the nav and the gate read it back.
test.describe.configure({ mode: "serial" });

const auth = { Authorization: `Bearer ${ADMIN_TOKEN}` };
const NAME = "unit-admin-view-profile";
let unitId = "";

test.beforeAll(async ({ request }) => {
  const made = await request.post("/api/v1/governance/units", {
    headers: auth,
    data: {
      name: NAME,
      ceiling: {
        allowed_domains: ["api.anthropic.com"],
        denied_domains: [],
        first_use_approval: "deny_with_review",
        min_confinement_class: "CC3",
        auto_stop_after_sec: 3600,
        eligible_grants: [],
        tool_rules: [{ tool: "*", effect: "allow" }],
      },
      limits: {},
    },
  });
  expect(made.status(), await made.text()).toBe(201);
  unitId = (await made.json()).unit.id;
  await grantUnitAdmin({ request }, unitId);
});

test.afterAll(async ({ request }) => {
  if (unitId) await request.delete(`/api/v1/governance/units/${unitId}`, { headers: auth });
});

test.describe("governance — a unit admin in the console", () => {
  test("a unit admin opens Governance from the nav", async ({ page }) => {
    await asRealUnitAdmin(page);
    const me = await consoleAPI(page, "GET", "/api/v1/me");
    expect(JSON.parse(me.text).unit_admin_of).toEqual(expect.arrayContaining([expect.objectContaining({ unit_id: unitId })]));

    await gotoConsole(page);
    await expect(sidebarLink(page, "Governance")).toBeVisible();
    await sidebarLink(page, "Governance").click();

    await expect(page).toHaveURL(/\/admin\/governance$/);
    await expect(page.getByRole("heading", { name: GOV.TITLE, level: 1 })).toBeVisible();
    await expect(page.getByText(NAME).first()).toBeVisible();
    await expect(page.getByRole("tab", { name: new RegExp(`^${CHANGES.TAB}`) })).toBeVisible();
    await expect(page.getByRole("heading", { name: VIEW_REFUSAL.TITLE })).toHaveCount(0);
  });

  test("a unit admin opens a profile read only", async ({ page }) => {
    await asRealUnitAdmin(page);
    await navToRoute(page, "/admin/governance");
    await expect(page.getByText(NAME).first()).toBeVisible();

    await expect(page.getByRole("button", { name: GOV.NEW_CTA })).toBeDisabled();
    await page.getByRole("button", { name: `${GOV.EDIT} ${NAME}` }).click();
    await expect(page.getByRole("heading", { name: GOV.EDITOR_TITLE_EDIT(NAME) })).toBeVisible();
    await expect(page.getByRole("button", { name: GOV.SAVE })).toBeDisabled();
  });

  test("a unit admin is refused the rest of the admin view", async ({ page }) => {
    await asRealUnitAdmin(page);
    await gotoConsole(page);
    for (const path of ["/admin/runs", "/admin/people"]) {
      await navToRoute(page, path);
      await expect(page.getByRole("heading", { name: VIEW_REFUSAL.TITLE })).toBeVisible();
    }
  });

  test("a user without a grant sees no Governance and no Audit", async ({ page }) => {
    await asRealMember(page);
    await gotoConsole(page);
    await expect(sidebarLink(page, "Governance")).toHaveCount(0);
    await expect(sidebarLink(page, "Audit")).toHaveCount(0);
    for (const path of ["/admin/governance", "/admin/audit"]) {
      await navToRoute(page, path);
      await expect(page.getByRole("heading", { name: VIEW_REFUSAL.TITLE })).toBeVisible();
      await expect(page.getByText(VIEW_REFUSAL.BODY)).toBeVisible();
    }
  });

  test("a unit admin opens Audit from the nav", async ({ page }) => {
    await asRealUnitAdmin(page);
    await gotoConsole(page);
    await expect(sidebarLink(page, "Audit")).toBeVisible();
    const audit = page.waitForRequest((r) => new URL(r.url()).pathname === "/api/v1/audit");
    await sidebarLink(page, "Audit").click();

    await expect(page).toHaveURL(/\/admin\/audit$/);
    await expect(page.getByRole("heading", { name: VIEW_REFUSAL.TITLE })).toHaveCount(0);
    await audit;
  });
});
