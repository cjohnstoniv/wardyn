/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { randomUUID } from "node:crypto";
import { test, expect, ADMIN_TOKEN, asRealSecurityAdmin, gotoConsole, navTo } from "./fixtures";
import { GOVERNANCE as GOV } from "../src/app/lib/governance-copy";
import { BOUNDARY, BOUNDARY_DRY_RUN } from "../src/app/lib/boundary-copy";
import { readSpec, SPEC_LABEL } from "./policy-source";
import type { Page } from "@playwright/test";

// The data_out rule in the three console editors. The reads each editor depends on are answered with page.route
// so the values are the ones the test names; the one real write, a profile's ceiling, carries the
// min_confinement_class the server refuses a create without.
const auth = { Authorization: `Bearer ${ADMIN_TOKEN}` };
const REVIEW_LINE =
  "Files leave one at a time through the Files panel, each with a person's yes. SSH file transfer, SSH commands, port forwards, run output, recording replay, web apps and computer folders are off.";

const DEFAULT_POLICY = {
  allowed_domains: ["api.anthropic.com"],
  first_use_approval: "deny_with_review",
  min_confinement_class: "CC1",
  restrictions: [],
};

async function stubDefaultPolicy(page: Page, over: Record<string, unknown>) {
  await page.route("**/api/v1/policies/default*", (route) => route.fulfill({ json: { ...DEFAULT_POLICY, ...over } }));
}

async function openNewPolicy(page: Page) {
  await gotoConsole(page, "admin");
  await navTo(page, "Policies");
  await page.getByRole("button", { name: "New policy" }).first().click();
  const dialog = page.getByRole("dialog").filter({ hasText: "New policy" });
  await expect(dialog).toBeVisible();
  return dialog;
}

test.describe("data_out in the editors", () => {
  test("the policy editor picks Review, shows its line, and writes it into the document", async ({ page }) => {
    await stubDefaultPolicy(page, {});
    const dialog = await openNewPolicy(page);
    const group = dialog.getByRole("radiogroup", { name: "Data out" });
    await expect(group).toBeVisible();
    await expect(dialog.getByText("Not set: allowed, unless a layer above holds content in.")).toBeVisible();
    expect(await readSpec(dialog.getByLabel(SPEC_LABEL))).not.toHaveProperty("data_out");
    await group.getByRole("radio", { name: "Review" }).click();
    await expect(group.getByRole("radio", { name: "Review" })).toHaveAttribute("aria-checked", "true");
    await expect(dialog.getByText(REVIEW_LINE)).toBeVisible();
    expect(await readSpec(dialog.getByLabel(SPEC_LABEL))).toMatchObject({ data_out: "review" });
  });

  test("a member whose ceiling is Review sees Allowed struck out, with the unit named", async ({ page }) => {
    await stubDefaultPolicy(page, { data_out: "review", unit_name: "Payments" });
    const dialog = await openNewPolicy(page);
    const group = dialog.getByRole("radiogroup", { name: "Data out" });
    await expect(dialog.getByText("Set above")).toBeVisible();
    await expect(group.getByRole("radio", { name: "Allowed" })).toBeDisabled();
    await expect(group.getByRole("radio", { name: "Review" })).toBeEnabled();
    await expect(group.getByRole("radio", { name: "Blocked" })).toBeEnabled();
    await expect(dialog.getByTestId("policy-data-out-floor")).toHaveText("Payments holds this at Review or stricter, so Allowed is not available.");
  });
});

test.describe("data_out in a profile's Ceiling", () => {
  test.describe.configure({ mode: "serial" });
  const suffix = randomUUID().slice(0, 8);
  const BASE_NAME = `data-out-base-${suffix}`;
  const KID_NAME = `data-out-kid-${suffix}`;
  let baseId = "";
  let kidId = "";

  test.beforeAll(async ({ request }) => {
    const base = await request.post("/api/v1/governance/units", {
      headers: auth,
      data: {
        name: BASE_NAME,
        ceiling: { allowed_domains: ["api.anthropic.com"], first_use_approval: "deny_with_review", min_confinement_class: "CC3", eligible_grants: [] },
      },
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

  test("the row inherits Allowed, switches on to Blocked, and the save carries it", async ({ page }) => {
    await asRealSecurityAdmin(page);
    let sent: { overlay?: Record<string, unknown> } | undefined;
    await page.route(`**/api/v1/governance/units/${kidId}`, async (route) => {
      if (route.request().method() !== "PUT") return route.fallback();
      sent = route.request().postDataJSON();
      await route.fallback();
    });
    await gotoConsole(page, "admin");
    await navTo(page, "Governance");
    await page.getByRole("table").first().getByRole("button", { name: `${GOV.EDIT} ${KID_NAME}` }).click();
    const editor = page.getByTestId("governance-profile-editor");
    const row = editor.getByTestId("governance-overlay-data_out");
    await expect(row.getByText("Inherited: Allowed")).toBeVisible();
    await row.getByRole("switch", { name: `data_out ${GOV.OVERLAY_NARROW}` }).click();
    await row.getByRole("button", { name: "Blocked" }).click();
    await expect(row.getByRole("button", { name: "Blocked" })).toHaveAttribute("aria-pressed", "true");
    await editor.getByRole("button", { name: GOV.SAVE }).click();
    await expect.poll(() => sent?.overlay?.data_out).toBe("blocked");
  });
});

test.describe("data_out on the Wardyn boundary", () => {
  test("the card saves Blocked and locked, and the PUT carries both", async ({ page }) => {
    await asRealSecurityAdmin(page);
    const stored = { mode: "hard_limits", spec: { denied_domains: ["paste.example.org"] }, limits: {}, locked_knobs: ["allowed_domains"] };
    let put: Record<string, unknown> | undefined;
    await page.route("**/api/v1/governance/boundary", async (route) => {
      const method = route.request().method();
      if (method === "GET") return route.fulfill({ json: stored, headers: { ETag: '"b1"' } });
      if (method !== "PUT") return route.fallback();
      put = route.request().postDataJSON();
      await route.fulfill({ json: put, headers: { ETag: '"b2"' } });
    });
    await page.route("**/api/v1/governance/dry-run", (route) =>
      route.fulfill({ json: { deployment: [], units: [], stored_policies: [], memberships: [], provider_rows: [] } }),
    );
    await gotoConsole(page, "admin");
    await navTo(page, "Policies");
    await page.getByRole("link", { name: BOUNDARY.TITLE, exact: true }).click();
    const card = page.getByTestId("boundary-data-out");
    await expect(card.getByText("Content leaving a run").first()).toBeVisible();
    const level = card.getByRole("group", { name: "Content leaving a run" });
    await level.getByText("Blocked", { exact: true }).click();
    await expect(level.getByRole("radio", { name: "Blocked" })).toBeChecked();
    await card.getByRole("checkbox", { name: "Lock for profiles below" }).check();
    await page.getByRole("button", { name: BOUNDARY.REVIEW, exact: true }).click();
    const report = page.getByTestId("boundary-dry-run");
    await report.getByRole("button", { name: BOUNDARY_DRY_RUN.SUBMIT_SAVE }).click();
    await expect.poll(() => put).toBeTruthy();
    expect(put).toMatchObject({
      mode: "hard_limits",
      spec: { denied_domains: ["paste.example.org"], data_out: "blocked" },
      locked_knobs: ["allowed_domains", "data_out"],
    });
  });
});
