/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #2514 — Make a policy from this run asks who can pick the policy, as Policies → New policy does.
// The sheet foot has one Save as policy; its dialog carries Available to (Everyone to begin with),
// and the list is written after the policy: Only these with a user type lands restricted and listed.
import { test, expect, ADMIN_TOKEN, gotoConsole, navTo } from "./fixtures";
import { AVAILABILITY } from "../src/app/lib/availability-copy";

const POLICY_NAME = `e2e-save-availability-${Date.now()}`;
const auth = { Authorization: `Bearer ${ADMIN_TOKEN}` };

test.describe("Run detail — Save as policy asks Available to (#2514)", () => {
  test.afterAll(async ({ request }) => {
    const res = await request.get("/api/v1/policies", { headers: auth });
    const policies: Array<{ id: string; name: string }> = await res.json();
    const created = policies.find((p) => p.name === POLICY_NAME);
    if (created) expect((await request.delete(`/api/v1/policies/${created.id}`, { headers: auth })).ok()).toBe(true);
  });

  test("one Save as policy button; the dialog asks Available to and writes the list", async ({ page }) => {
    await gotoConsole(page, "admin");
    await navTo(page, "Runs");
    await page.getByText("e2e fixture 4").click();
    await expect(page).toHaveURL(/\/runs\/[0-9a-f-]{8,}/);
    await page.getByRole("tab", { name: "Audit" }).click();
    await page.getByRole("button", { name: "Make a policy from this run" }).click();

    const sheet = page.getByRole("dialog").filter({ hasText: "Synthesized profile" });
    await expect(sheet.getByText("Overall risk")).toBeVisible();
    await expect(sheet.getByRole("button", { name: /^Save as policy$/ })).toHaveCount(1);
    await expect(sheet.getByText(/Save as is/)).toHaveCount(0);

    await sheet.getByRole("button", { name: /^Save as policy$/ }).click();
    const dialog = page.getByRole("dialog").filter({ hasText: "Persist the synthesized inline_policy" });
    await dialog.getByLabel("Policy name").fill(POLICY_NAME);
    await expect(dialog.getByText(AVAILABILITY.LABEL)).toBeVisible();
    await expect(dialog.getByRole("radio", { name: AVAILABILITY.EVERYONE })).toBeChecked();

    // Only these with nobody listed holds Save policy shut; "standard" is the built-in user type.
    await dialog.getByRole("radio", { name: AVAILABILITY.ONLY }).click();
    await expect(dialog.getByRole("button", { name: "Save policy" })).toBeDisabled();
    await dialog.getByPlaceholder(AVAILABILITY.ADD_PLACEHOLDER).fill("standard");
    await dialog.getByRole("button", { name: AVAILABILITY.ADD_CTA, exact: true }).click();
    await expect(dialog.getByText("Standard user")).toBeVisible();
    await dialog.getByRole("button", { name: "Save policy" }).click();
    await expect(dialog).toHaveCount(0);
    await expect(sheet).toHaveCount(0);

    const policies: Array<{ id: string; name: string }> = await (
      await page.request.get("/api/v1/policies", { headers: auth })
    ).json();
    const created = policies.find((p) => p.name === POLICY_NAME);
    expect(created).toBeTruthy();
    const view = await (
      await page.request.get(`/api/v1/permissions/availability/policy/${created!.id}`, { headers: auth })
    ).json();
    expect(view.restricted).toBe(true);
    expect(view.allowed_by).toEqual([
      expect.objectContaining({ subject_type: "user_type", subject: "standard", capability: "policy", value: created!.id }),
    ]);
  });
});
