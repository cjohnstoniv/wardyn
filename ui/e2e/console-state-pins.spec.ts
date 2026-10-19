/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, gotoConsole, navTo, navToRoute } from "./fixtures";
import type { Page } from "@playwright/test";
import { GOVERNANCE as GOV, PERM, PREVIEW } from "../src/app/lib/governance-copy";
import { aheadByHours } from "../src/app/lib/test-clock";

// Screens that must not act on an answer, or fail to act on a refusal, that no longer belongs to what is on
// screen: the Governance previews and delete confirms. The snapshot is stubbed at its route; the writes are held
// or refused by the stub so the race is the test, not the timing.

const CEILING = { allowed_domains: ["api.anthropic.com"], first_use_approval: "deny_with_review", min_confinement_class: "CC2" };
const profile = (id: string, name: string) => ({
  id, name, ceiling: CEILING, limits: {}, created_at: aheadByHours(-1), updated_at: aheadByHours(-1),
});
const PLATFORM = profile("p-platform", "Platform");
const GREENFIELD = profile("p-greenfield", "Greenfield contractors");
const ASSIGNMENT = {
  id: "a-platform", subject_type: "group", subject: "wardyn.platform", profile_id: PLATFORM.id, priority: 10, created_at: aheadByHours(-1),
};

async function openGovernance(page: Page, state: { profiles: unknown[]; assignments: unknown[] }): Promise<{ reads: () => number }> {
  let reads = 0;
  await page.route("**/api/v1/governance", (route) => {
    if (route.request().method() !== "GET") return route.fallback();
    reads += 1;
    return route.fulfill({ json: state });
  });
  await page.route("**/api/v1/governance/changes", (route) => route.fulfill({ json: [] }));
  await gotoConsole(page, "admin");
  await navTo(page, "Governance");
  await expect(page.getByRole("heading", { name: GOV.TITLE, level: 1 })).toBeVisible();
  return { reads: () => reads };
}

test.describe("Governance: an answer or a refusal is acted on once, and only for what is on screen", () => {
  test("the unassign confirm is disabled while its delete is in flight", async ({ page }) => {
    let release!: () => void;
    const held = new Promise<void>((resolve) => (release = resolve));
    let deletes = 0;
    await page.route("**/api/v1/governance/assignments/a-platform", async (route) => {
      deletes += 1;
      await held;
      await route.fulfill({ status: 204, body: "" });
    });
    await openGovernance(page, { profiles: [PLATFORM], assignments: [ASSIGNMENT] });

    await page.getByRole("button", { name: `${PERM.REMOVE} wardyn.platform` }).click();
    const confirm = page.getByRole("alertdialog").getByRole("button", { name: PERM.REMOVE, exact: true });
    await confirm.click();
    await expect(confirm).toBeDisabled();
    release();
    expect(deletes).toBe(1);
  });

  test("a refused profile delete re-reads the list", async ({ page }) => {
    await page.route("**/api/v1/governance/profiles/p-greenfield", (route) =>
      route.fulfill({ status: 409, json: { error: "this governance profile is still assigned", reason: "governance_profile_in_use" } }),
    );
    const governance = await openGovernance(page, { profiles: [GREENFIELD], assignments: [] });
    const before = governance.reads();

    await page.getByRole("button", { name: `${GOV.DELETE} ${GREENFIELD.name}` }).click();
    const dialog = page.getByRole("alertdialog");
    await dialog.getByRole("button", { name: GOV.DELETE, exact: true }).click();
    await expect(dialog.getByText(GOV.DELETE_RESTRICT_TITLE)).toBeVisible();
    await expect.poll(governance.reads).toBe(before + 1);
  });

  test("an answer to claims that were edited while it was in flight is not shown under the new claims", async ({ page }) => {
    let release!: () => void;
    const held = new Promise<void>((resolve) => (release = resolve));
    await page.route("**/api/v1/governance/preview", async (route) => {
      await held;
      await route.fulfill({ json: { profile_id: PLATFORM.id, profile_name: PLATFORM.name, matched_tier: "group" } });
    });
    await openGovernance(page, { profiles: [PLATFORM], assignments: [] });

    const claims = page.getByLabel(PREVIEW.FIELD_CLAIMS);
    await claims.fill("wardyn.platform");
    const answered = page.waitForResponse((r) => r.url().includes("/governance/preview"));
    await page.getByRole("button", { name: GOV.PREVIEW_RUN_CTA }).click();
    await claims.fill("wardyn.other");
    release();
    await answered;
    await page.evaluate(() => new Promise((done) => requestAnimationFrame(() => requestAnimationFrame(done))));
    await expect(page.getByText(GOV.PREVIEW_RESULT(PLATFORM.name, GOV.MATCHED_GROUP))).toHaveCount(0);
  });
});

test.describe("Components: a delete is sent once, and its dialog holds until it answers", () => {
  test("Escape and a second click during a held delete neither dismiss nor re-send", async ({ page }) => {
    const component = {
      id: "11111111-1111-4111-8111-111111111111",
      name: "Payments API",
      version: 1,
      created_at: aheadByHours(-24),
      updated_at: aheadByHours(-24),
      definition: { hosts: ["api.pay.example"] },
    };
    let gone = false;
    await page.route("**/api/v1/components", (route) =>
      route.request().method() === "GET" ? route.fulfill({ json: gone ? [] : [component] }) : route.fallback(),
    );
    let release!: () => void;
    const held = new Promise<void>((resolve) => (release = resolve));
    let deletes = 0;
    await page.route(`**/api/v1/components/${component.id}`, async (route) => {
      if (route.request().method() !== "DELETE") return route.fallback();
      deletes += 1;
      await held;
      gone = true;
      await route.fulfill({ status: 204, body: "" });
    });
    await gotoConsole(page, "admin");
    await navToRoute(page, "/admin/components");

    await page.getByRole("button", { name: "Delete Payments API" }).click();
    const dialog = page.getByRole("alertdialog");
    const confirm = dialog.getByRole("button", { name: "Delete component" });
    await confirm.click();
    await expect(confirm).toBeDisabled();
    await page.keyboard.press("Escape");
    await expect(dialog).toBeVisible();
    release();
    await expect(dialog).toHaveCount(0);
    expect(deletes).toBe(1);
  });
});
