/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { randomUUID } from "node:crypto";
import { test, expect, ADMIN_TOKEN, consoleAPI, gotoConsole, navTo, asRealSecurityAdmin } from "./fixtures";
import { DECIDERS, DECIDER_KIND_LABEL } from "../src/app/components/wardyn/copy/deciders";
import { GOVERNANCE as GOV } from "../src/app/lib/governance-copy";

// A security admin sets who besides security admins decides each kind of hold for a profile. The four-eyes
// switch is off in this suite, so a security admin's write applies. The admin token is only the setup and
// cleanup writer. Names carry a run id so a leftover from another spec never matches.
test.describe.configure({ mode: "serial" });

const auth = { Authorization: `Bearer ${ADMIN_TOKEN}` };
const RUN = randomUUID().slice(0, 8);
const NAME_P = `deciders-parent-${RUN}`;
const NAME_C = `deciders-child-${RUN}`;
const LEADS = `e2e-leads-${RUN}`;
const CEILING = {
  allowed_domains: ["api.anthropic.com"],
  denied_domains: [],
  first_use_approval: "deny_with_review",
  min_confinement_class: "CC3",
  auto_stop_after_sec: 3600,
  eligible_grants: [],
  tool_rules: [{ tool: "*", effect: "allow" }],
};

const ids: Record<string, string> = {};

async function openEditor(page: import("@playwright/test").Page, name: string) {
  await asRealSecurityAdmin(page);
  await gotoConsole(page, "admin");
  await navTo(page, "Governance");
  await page.getByRole("button", { name: `${GOV.EDIT} ${name}`, exact: true }).click();
  await expect(page.getByRole("heading", { name: DECIDERS.TITLE, level: 4 })).toBeVisible();
}

const box = (page: import("@playwright/test").Page, i: number) => page.getByTestId(`decider-rule-editor-${i}`);

test.describe("governance — profile deciders", () => {
  test.afterAll(async ({ request }) => {
    for (const id of [ids.c, ids.p]) {
      if (id) await request.delete(`/api/v1/governance/units/${id}`, { headers: auth });
    }
  });

  test("setup: a profile and a child based on it", async ({ request }) => {
    const make = async (data: object) => {
      const res = await request.post("/api/v1/governance/units", { headers: auth, data });
      expect(res.status(), await res.text()).toBe(201);
      return (await res.json()).unit.id as string;
    };
    ids.p = await make({ name: NAME_P, ceiling: CEILING, limits: {} });
    ids.c = await make({ name: NAME_C, parent_id: ids.p, mode: "pass_through" });
  });

  test("a security admin sets a network egress rule and the snapshot shows it", async ({ page }) => {
    await openEditor(page, NAME_P);
    await page.getByRole("button", { name: DECIDERS.ADD, exact: true }).click();
    const rule = box(page, 0);
    await rule.getByRole("checkbox", { name: new RegExp(DECIDER_KIND_LABEL.egress_domain) }).click();
    await rule.getByRole("checkbox", { name: new RegExp(DECIDERS.WHO_OWNER) }).click();
    await rule.getByPlaceholder(DECIDERS.GROUP_PLACEHOLDER).fill(LEADS);
    await rule.getByRole("button", { name: DECIDERS.ADD_GROUP, exact: true }).click();
    await rule.getByText(DECIDERS.SCOPE_ONCE, { exact: true }).click();
    await rule.getByRole("button", { name: DECIDERS.DONE, exact: true }).click();
    await page.getByRole("button", { name: DECIDERS.SAVE, exact: true }).click();
    await expect(page.getByText(DECIDERS.SAVED, { exact: true })).toBeVisible();

    const res = await consoleAPI(page, "GET", "/api/v1/governance");
    expect(res.status, res.text).toBe(200);
    const mine = (JSON.parse(res.text).deciders as Array<{ unit_id: string; rules: unknown[] }>).find((d) => d.unit_id === ids.p);
    expect(mine?.rules).toEqual([{ kinds: ["egress_domain"], who: ["owner", `group:${LEADS}`], max_scope: "once" }]);
  });

  test("a rule wider than the one above is refused with the server's sentence, and the rule opens", async ({ page }) => {
    await openEditor(page, NAME_C);
    await expect(page.getByText(DECIDERS.FROM(NAME_P), { exact: true })).toBeVisible();
    await page.getByRole("button", { name: DECIDERS.SET_HERE, exact: true }).click();
    const rule = box(page, 0);
    await rule.getByRole("checkbox", { name: new RegExp(DECIDER_KIND_LABEL.egress_domain) }).click();
    await rule.getByPlaceholder(DECIDERS.GROUP_PLACEHOLDER).fill("other");
    await rule.getByRole("button", { name: DECIDERS.ADD_GROUP, exact: true }).click();
    await rule.getByRole("button", { name: DECIDERS.DONE, exact: true }).click();
    await page.getByRole("button", { name: DECIDERS.SAVE, exact: true }).click();

    const note = page.getByRole("alert").filter({ hasText: DECIDERS.REFUSED_TITLE });
    await expect(note).toBeVisible();
    await expect(note).toContainText("rules[0]");
    await expect(note).toContainText("reaches further");
    await expect(box(page, 0)).toBeVisible();
  });

  test("a lock above makes the rules read only", async ({ page, request }) => {
    const res = await request.put(`/api/v1/governance/units/${ids.p}`, {
      headers: auth,
      data: { name: NAME_P, ceiling: CEILING, limits: {}, locked_knobs: ["deciders"] },
    });
    expect(res.status(), await res.text()).toBe(200);

    await openEditor(page, NAME_C);
    await expect(page.getByText(DECIDERS.LOCKED_BY(NAME_P), { exact: true })).toBeVisible();
    await expect(page.getByRole("button", { name: DECIDERS.ADD, exact: true })).toHaveCount(0);
    await expect(page.getByRole("button", { name: DECIDERS.SET_HERE, exact: true })).toHaveCount(0);
  });

  test("an elevation rule is its own rule, with no owner, no scope and a bounded limit", async ({ page }) => {
    await openEditor(page, NAME_P);
    await page.getByRole("button", { name: DECIDERS.ADD, exact: true }).click();
    const rule = box(page, 1);
    await rule.getByRole("checkbox", { name: new RegExp(DECIDER_KIND_LABEL.elevation) }).click();
    await expect(rule.getByRole("checkbox", { name: new RegExp(DECIDERS.WHO_OWNER) })).toBeDisabled();
    await expect(rule.getByText(DECIDERS.OWNER_OFF_REQUEST, { exact: true })).toBeVisible();
    await expect(rule.getByText(DECIDERS.SCOPE_LABEL, { exact: true })).toHaveCount(0);
    await rule.getByLabel(DECIDERS.ELEV_MAX_LABEL, { exact: true }).fill("10");
    await expect(rule.getByText(DECIDERS.ELEV_MAX_ERROR, { exact: true })).toBeVisible();
    await expect(rule.getByRole("button", { name: DECIDERS.DONE, exact: true })).toBeDisabled();
  });
});
