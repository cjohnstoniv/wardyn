/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { randomUUID } from "node:crypto";
import { test, expect, ADMIN_TOKEN, gotoConsole, navTo, asRealSecurityAdmin } from "./fixtures";
import { ENVELOPE } from "../src/app/components/wardyn/copy/envelope";
import { POOL_DRAWER } from "../src/app/components/wardyn/copy/runner-pools";

// A security admin marks a platform's root, names the groups approved for it, and removes the envelope.
// The four-eyes switch is off in this suite, so every write applies. The admin token is only the setup
// and cleanup writer. Names carry a run id so a leftover from another spec never matches.
test.describe.configure({ mode: "serial" });

const auth = { Authorization: `Bearer ${ADMIN_TOKEN}` };
const RUN = randomUUID().slice(0, 8);
const NAME_R = `env-root-${RUN}`;
const NAME_K = `env-kid-${RUN}`;
const NAME_S = `env-other-${RUN}`;
const GROUP_A = `e2e-plat-a-${RUN}`;
const GROUP_B = `e2e-plat-b-${RUN}`;
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

const cardRow = (page: import("@playwright/test").Page, name: string) =>
  page.getByRole("row").filter({ has: page.getByRole("button", { name: `${ENVELOPE.EDIT} ${name}`, exact: true }) });

test.describe("governance — platform envelopes", () => {
  test("setup: a root, a profile below it, and a second root", async ({ request }) => {
    const make = async (data: object) => {
      const res = await request.post("/api/v1/governance/units", { headers: auth, data });
      expect(res.status(), await res.text()).toBe(201);
      return (await res.json()).unit.id as string;
    };
    ids.r = await make({ name: NAME_R, ceiling: CEILING, limits: {} });
    ids.k = await make({ name: NAME_K, parent_id: ids.r, mode: "pass_through" });
    ids.s = await make({ name: NAME_S, ceiling: CEILING, limits: {} });
  });

  test("a security admin sets an envelope and the Profiles table marks the root and what is below it", async ({ page }) => {
    await asRealSecurityAdmin(page);
    await gotoConsole(page, "admin");
    await navTo(page, "Governance");
    await expect(page.getByRole("heading", { name: ENVELOPE.TITLE })).toBeVisible();

    await page.getByRole("button", { name: ENVELOPE.SET, exact: true }).first().click();
    await page.getByRole("combobox", { name: ENVELOPE.ROOT_LABEL }).click();
    await page.getByRole("option", { name: NAME_R, exact: true }).click();
    await page.getByLabel(ENVELOPE.GROUPS_LABEL).fill(GROUP_A);
    await page.getByRole("button", { name: ENVELOPE.SAVE_ENVELOPE, exact: true }).click();

    await expect(cardRow(page, NAME_R)).toContainText(GROUP_A);
    await expect(page.getByText(ENVELOPE.CHIP_ROOT, { exact: true })).toHaveCount(1);
    await expect(page.getByText(ENVELOPE.CHIP_IN(NAME_R), { exact: true })).toHaveCount(1);
  });

  test("Edit groups adds a second group", async ({ page }) => {
    await asRealSecurityAdmin(page);
    await gotoConsole(page, "admin");
    await navTo(page, "Governance");

    await page.getByRole("button", { name: `${ENVELOPE.EDIT} ${NAME_R}`, exact: true }).click();
    await page.getByLabel(ENVELOPE.ADD_LABEL).fill(GROUP_B);
    await page.getByRole("button", { name: POOL_DRAWER.ADD_TO_LIST, exact: true }).click();
    await page.getByRole("button", { name: ENVELOPE.SAVE_GROUPS, exact: true }).click();

    await expect(cardRow(page, NAME_R)).toContainText(GROUP_A);
    await expect(cardRow(page, NAME_R)).toContainText(GROUP_B);
  });

  test("a group already on one envelope is refused on another, with the server's sentence", async ({ page }) => {
    await asRealSecurityAdmin(page);
    await gotoConsole(page, "admin");
    await navTo(page, "Governance");

    await page.getByRole("button", { name: ENVELOPE.SET, exact: true }).first().click();
    await page.getByRole("combobox", { name: ENVELOPE.ROOT_LABEL }).click();
    await page.getByRole("option", { name: NAME_S, exact: true }).click();
    await page.getByLabel(ENVELOPE.GROUPS_LABEL).fill(GROUP_A);
    await page.getByRole("button", { name: ENVELOPE.SAVE_ENVELOPE, exact: true }).click();

    const note = page.getByRole("alert").filter({ hasText: ENVELOPE.REFUSED_TITLE });
    await expect(note).toBeVisible();
    await expect(note).toContainText(`the group "${GROUP_A}" is already approved on the envelope of unit "${NAME_R}"`);
    await expect(note).toContainText("a group sits on at most one envelope");
    await expect(cardRow(page, NAME_S)).toHaveCount(0);
  });

  test("Remove takes the envelope and its chips away", async ({ page }) => {
    await asRealSecurityAdmin(page);
    await gotoConsole(page, "admin");
    await navTo(page, "Governance");

    await page.getByRole("button", { name: `${ENVELOPE.REMOVE} ${NAME_R}`, exact: true }).click();
    const dialog = page.getByRole("alertdialog");
    await expect(dialog).toContainText(ENVELOPE.REMOVE_TITLE(NAME_R));
    await dialog.getByRole("button", { name: ENVELOPE.REMOVE_CONFIRM, exact: true }).click();

    await expect(cardRow(page, NAME_R)).toHaveCount(0);
    await expect(page.getByText(ENVELOPE.CHIP_ROOT, { exact: true })).toHaveCount(0);
    await expect(page.getByText(ENVELOPE.CHIP_IN(NAME_R), { exact: true })).toHaveCount(0);
  });

  test("cleanup: the envelopes and the profiles", async ({ request }) => {
    for (const id of [ids.r, ids.s]) {
      if (id) await request.delete(`/api/v1/governance/units/${id}/envelope`, { headers: auth });
    }
    for (const id of [ids.k, ids.r, ids.s]) {
      if (id) await request.delete(`/api/v1/governance/units/${id}`, { headers: auth });
    }
  });
});
