/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Admin Settings, macOS virtual machines: the oldest macOS base, `macos_vm.max_base_age_days` of the site
// config. Real backend, no stub: what is pinned is that the daemon accepts the body the field sends, stores
// it, and reads it back as the field's value, then that clearing the field removes the maximum. The wording is
// read from the copy module. Serial, and the maximum is cleared before the test ends: one backend, and a
// maximum left behind would refuse other specs' Mac runs.
import { test, expect, consoleAPI, expandCard, gotoConsole, navToRoute } from "./fixtures";
import { SERVER_OWNED_SITE_CONFIG_KEYS } from "../src/app/lib/types/site";
import { FORM_ACTIONS } from "../src/app/components/wardyn/copy/form-actions";
import { MAC_SETTINGS } from "../src/app/components/wardyn/copy/mac-settings";

test.describe.configure({ mode: "serial" });

const stored = async (page: import("@playwright/test").Page) => {
  const res = await consoleAPI(page, "GET", "/api/v1/site-config");
  expect(res.status, res.text).toBe(200);
  return (JSON.parse(res.text) as { macos_vm?: { max_base_age_days?: number } }).macos_vm?.max_base_age_days;
};

test("the oldest macOS base saves, reloads, and clears", async ({ page }) => {
  await gotoConsole(page, "admin");
  await navToRoute(page, "/admin/settings");
  await expandCard(page, MAC_SETTINGS.TITLE);
  const field = page.getByRole("textbox", { name: MAC_SETTINGS.P17 });
  const save = page.getByRole("button", { name: FORM_ACTIONS.SAVE, exact: true });
  try {
    await expect(page.getByText(MAC_SETTINGS.P18, { exact: true })).toBeVisible();
    await expect(field).toHaveValue("");

    await field.fill("30");
    await save.click();
    await expect(page.getByText(MAC_SETTINGS.SAVED_TOAST)).toBeVisible();
    expect(await stored(page)).toBe(30);

    await page.reload();
    await expandCard(page, MAC_SETTINGS.TITLE);
    await expect(field).toHaveValue("30");

    await field.fill("0");
    await expect(page.getByTestId("macos-vm-settings-card").getByRole("alert")).toHaveText(MAC_SETTINGS.INVALID);
    await expect(save).toBeDisabled();

    await field.fill("");
    await save.click();
    await expect.poll(() => stored(page)).toBeUndefined();
  } finally {
    // Whatever failed above, leave no maximum behind: the stored document, minus what the server owns, with an empty block.
    const body = JSON.parse((await consoleAPI(page, "GET", "/api/v1/site-config")).text) as Record<string, unknown>;
    for (const k of SERVER_OWNED_SITE_CONFIG_KEYS) delete body[k];
    await consoleAPI(page, "PUT", "/api/v1/site-config", { ...body, macos_vm: {} });
  }
});
