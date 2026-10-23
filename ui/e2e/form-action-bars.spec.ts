/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { Locator } from "@playwright/test";
import { test, expect, asRealSecurityAdmin, expandCard, gotoConsole, navTo } from "./fixtures";
import { BRANDING } from "../src/app/lib/branding-copy";
import { COMPONENTS_ADMIN } from "../src/app/components/wardyn/copy/components-admin";
import { GOVERNANCE as GOV } from "../src/app/lib/governance-copy";
import { PERM } from "../src/app/lib/permissions-copy";
import { BASELINE } from "../src/app/lib/governance-copy";
import { PROVIDER_EDITOR } from "../src/app/lib/model-providers-copy";
import { USER_TYPES as UT } from "../src/app/lib/user-types-copy";

// #3064: every short form ends the same way, actions at the right end with the one primary last. The bars are read
// by geometry (the button's right edge against its bar's), which is what a person sees.

async function atTheRightEnd(button: Locator) {
  await expect(button).toBeVisible();
  const b = (await button.boundingBox())!;
  const bar = (await button.locator("xpath=..").boundingBox())!;
  expect(Math.abs(bar.x + bar.width - (b.x + b.width))).toBeLessThanOrEqual(1);
}

test.describe("form action bars (#3064)", () => {
  test("Settings: Remove branding far left, Save branding last on the right; Save component settings full size on the right", async ({ page }) => {
    await page.route("**/api/v1/branding/settings", (route) =>
      route.request().method() === "GET"
        ? route.fulfill({ json: { org_name: "Example Corp", name_format: "prefix", primary: "#7c3aed", primary_text: "#ffffff" } })
        : route.fallback(),
    );
    await gotoConsole(page, "admin");
    await page.goto("/admin/settings");

    await expandCard(page, BRANDING.TITLE);
    const save = page.getByRole("button", { name: BRANDING.SAVE, exact: true });
    const remove = page.getByRole("button", { name: BRANDING.REMOVE_BRANDING, exact: true });
    await atTheRightEnd(save);
    const bar = (await save.locator("xpath=..").boundingBox())!;
    const r = (await remove.boundingBox())!;
    const s = (await save.boundingBox())!;
    expect(r.x).toBeLessThan(s.x);
    expect(Math.abs(r.x - bar.x)).toBeLessThanOrEqual(1);

    await expandCard(page, "Custom components");
    const comp = page.getByRole("button", { name: COMPONENTS_ADMIN.SETTINGS_SAVE, exact: true });
    await atTheRightEnd(comp);
    expect((await comp.boundingBox())!.height).toBeGreaterThanOrEqual(36);
    await page.unrouteAll({ behavior: "ignoreErrors" });
  });

  test("Permissions: Add grant sits at the right end of its form", async ({ page }) => {
    await gotoConsole(page, "admin");
    await navTo(page, "Permissions");
    await atTheRightEnd(page.getByRole("button", { name: PERM.ADD_CTA, exact: true }));
  });

  test("Governance: Assign profile sits at the right end of its form", async ({ page }) => {
    await gotoConsole(page, "admin");
    await navTo(page, "Governance");
    await atTheRightEnd(page.getByRole("button", { name: GOV.ADD_CTA, exact: true }));
  });

  // Left to right: `first` ends before `second` begins, on the same bar.
  async function inOrder(first: Locator, second: Locator) {
    const a = (await first.boundingBox())!;
    const b = (await second.boundingBox())!;
    expect(a.x + a.width).toBeLessThanOrEqual(b.x + 1);
  }

  test("User types: Cancel then Save type, on the right", async ({ page }) => {
    await gotoConsole(page, "admin");
    await navTo(page, "User types");
    await page.getByRole("button", { name: UT.NEW_CTA, exact: true }).click();
    const editor = page.getByTestId("user-type-editor");
    const save = editor.getByRole("button", { name: UT.SAVE, exact: true });
    await atTheRightEnd(save);
    await inOrder(editor.getByRole("button", { name: UT.CANCEL, exact: true }), save);
  });

  test("Egress baseline: the filled Save hosts is at the right end", async ({ page }) => {
    await asRealSecurityAdmin(page);
    await gotoConsole(page, "admin");
    await navTo(page, "Governance");
    await page.getByRole("tab", { name: BASELINE.TAB }).click();
    await atTheRightEnd(page.getByRole("button", { name: BASELINE.SAVE, exact: true }));
  });

  test("Model provider: Cancel then Save provider, on the right", async ({ page }) => {
    await gotoConsole(page, "admin");
    await page.goto("/admin/settings");
    await expandCard(page, "Model providers");
    await page.getByRole("button", { name: "Add model provider" }).click();
    const dialog = page.getByRole("dialog");
    await dialog.getByRole("button", { name: "Amazon Bedrock" }).click();
    const save = dialog.getByRole("button", { name: PROVIDER_EDITOR.SAVE, exact: true });
    await expect(save).toBeVisible();
    await inOrder(dialog.getByRole("button", { name: PROVIDER_EDITOR.CANCEL, exact: true }), save);
    const bar = (await save.locator("xpath=..").boundingBox())!;
    const s = (await save.boundingBox())!;
    expect(Math.abs(bar.x + bar.width - (s.x + s.width))).toBeLessThanOrEqual(1);
  });
});
