/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { randomUUID } from "node:crypto";
import { test, expect, ADMIN_TOKEN, asRealSecurityAdmin, gotoConsole, navTo } from "./fixtures";
import { GOVERNANCE as GOV } from "../src/app/lib/governance-copy";
import { DENY_LISTS } from "../src/app/components/wardyn/copy/deny-lists";

// The deny lists against the real server. A standalone profile R denies one component id; a composed profile K sits
// on it. Narrowing K's list starts empty and only adds, a name where an id belongs is refused under its own
// heading, and a standalone profile has both rows. The name strips need the operator reads, so they are pinned in
// the component suite beside the strip.
const auth = { Authorization: `Bearer ${ADMIN_TOKEN}` };
const suffix = randomUUID().slice(0, 8);
const BASE_NAME = `deny-base-${suffix}`;
const KID_NAME = `deny-kid-${suffix}`;
const DENIED = randomUUID();
const CEILING = {
  allowed_domains: ["api.anthropic.com"],
  first_use_approval: "deny_with_review",
  min_confinement_class: "CC3",
  eligible_grants: [],
  tool_rules: [{ tool: "*", effect: "allow" }],
};

test.describe("governance — the deny lists", () => {
  test.describe.configure({ mode: "serial" });
  let baseId = "";
  let kidId = "";

  test.beforeAll(async ({ request }) => {
    const base = await request.post("/api/v1/governance/units", {
      headers: auth,
      data: { name: BASE_NAME, ceiling: CEILING, limits: { deny_components: [DENIED] } },
    });
    expect(base.status(), await base.text()).toBe(201);
    baseId = (await base.json()).unit.id as string;
    const kid = await request.post("/api/v1/governance/units", { headers: auth, data: { name: KID_NAME, parent_id: baseId, overlay: {} } });
    expect(kid.status(), await kid.text()).toBe(201);
    kidId = (await kid.json()).unit.id as string;
  });

  test.afterAll(async ({ request }) => {
    if (kidId) await request.delete(`/api/v1/governance/units/${kidId}`, { headers: auth });
    if (baseId) await request.delete(`/api/v1/governance/units/${baseId}`, { headers: auth });
  });

  test("narrowing a deny list starts empty and a bad entry is refused", async ({ page }) => {
    await asRealSecurityAdmin(page);
    await gotoConsole(page, "admin");
    await navTo(page, "Governance");
    await page.getByRole("table").first().getByRole("button", { name: `${GOV.EDIT} ${KID_NAME}` }).click();
    const editor = page.getByTestId("governance-profile-editor");
    await editor.getByRole("switch", { name: `${DENY_LISTS.COMPONENTS_LABEL} ${GOV.OVERLAY_NARROW}` }).click();
    const box = editor.getByLabel(DENY_LISTS.COMPONENTS_LABEL, { exact: true });
    await expect(box).toHaveValue("");
    await expect(editor.getByText(GOV.OVERLAY_INHERITED(DENIED))).toBeVisible();
    await expect(box).toHaveAttribute("placeholder", DENY_LISTS.PLACEHOLDER);
    await box.fill("not-an-id");
    await editor.getByRole("button", { name: GOV.SAVE }).click();
    const refusal = editor.getByRole("alert").filter({ hasText: DENY_LISTS.REFUSED_LIST });
    await expect(refusal).toBeVisible();
    await expect(refusal).toContainText("deny_components:");
    await expect(refusal).toContainText("is not an id");
  });

  test("a standalone profile has both deny rows", async ({ page }) => {
    await asRealSecurityAdmin(page);
    await gotoConsole(page, "admin");
    await navTo(page, "Governance");
    await page.getByRole("table").first().getByRole("button", { name: `${GOV.EDIT} ${BASE_NAME}` }).click();
    const editor = page.getByTestId("governance-profile-editor");
    await expect(editor.getByLabel(DENY_LISTS.COMPONENTS_LABEL, { exact: true })).toHaveValue(DENIED);
    await expect(editor.getByLabel(DENY_LISTS.PROVIDERS_LABEL, { exact: true })).toHaveValue("");
  });
});
