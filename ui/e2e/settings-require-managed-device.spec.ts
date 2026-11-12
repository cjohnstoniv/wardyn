/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { Page } from "@playwright/test";
import { test, expect, gotoConsole, navToRoute } from "./fixtures";
import { RUNNER, RUNNERS } from "../src/app/components/wardyn/copy/runners";

// Settings > Runners: the "Require a managed device" switch. The harness has runners settings of its own, so the
// route is answered here: GET gives the stored value, PUT records the body and stores what it names. What is
// pinned is the console: a confirm for On, one click for Off, only require_managed_device on the wire, the
// refusal inside the dialog, and the failed read with its retry.
type Stored = { enabled: boolean; require_managed_device?: boolean };

async function stubSettings(page: Page, start: Stored, opts: { putStatus?: number; getStatuses?: number[] } = {}) {
  let stored = start;
  const puts: unknown[] = [];
  const getStatuses = [...(opts.getStatuses ?? [])];
  await page.route("**/api/v1/runners/settings", async (route) => {
    if (route.request().method() === "PUT") {
      const body = route.request().postDataJSON() as Partial<Stored>;
      puts.push(body);
      if (opts.putStatus) return route.fulfill({ status: opts.putStatus, json: { error: "forbidden" } });
      stored = { ...stored, ...body };
      return route.fulfill({ json: stored });
    }
    const status = getStatuses.shift() ?? 200;
    return status === 200 ? route.fulfill({ json: stored }) : route.fulfill({ status, json: { error: "boom" } });
  });
  return puts;
}

async function openCard(page: Page) {
  await gotoConsole(page, "admin");
  await navToRoute(page, "/admin/settings");
  await page.getByRole("button", { name: new RegExp(`^${RUNNERS.MANAGED_CARD_TITLE}( |$)`) }).click();
  return page.getByTestId("runners-settings-card");
}

test.describe("Settings: Require a managed device", () => {
  test("off, then on after a confirm: only require_managed_device is sent and the switch reads the answer", async ({ page }) => {
    const puts = await stubSettings(page, { enabled: true });
    const card = await openCard(page);
    const sw = card.getByRole("switch", { name: RUNNERS.MANAGED_LABEL });
    await expect(sw).toHaveAttribute("aria-checked", "false");
    await sw.click();
    const dialog = page.getByRole("alertdialog", { name: RUNNERS.MANAGED_CONFIRM_TITLE });
    await expect(dialog.getByText(RUNNERS.MANAGED_CONFIRM_BODY)).toBeVisible();
    expect(puts).toEqual([]);
    await dialog.getByRole("button", { name: RUNNERS.MANAGED_CONFIRM }).click();
    await expect(sw).toHaveAttribute("aria-checked", "true");
    expect(puts).toEqual([{ require_managed_device: true }]);
  });

  test("turning off sends false with no dialog", async ({ page }) => {
    const puts = await stubSettings(page, { enabled: true, require_managed_device: true });
    const card = await openCard(page);
    const sw = card.getByRole("switch", { name: RUNNERS.MANAGED_LABEL });
    await expect(sw).toHaveAttribute("aria-checked", "true");
    await sw.click();
    await expect(sw).toHaveAttribute("aria-checked", "false");
    await expect(page.getByRole("alertdialog")).toHaveCount(0);
    expect(puts).toEqual([{ require_managed_device: false }]);
  });

  test("a 403 on confirm shows the heading and the super admin sentence inside the dialog", async ({ page }) => {
    await stubSettings(page, { enabled: true }, { putStatus: 403 });
    const card = await openCard(page);
    await card.getByRole("switch", { name: RUNNERS.MANAGED_LABEL }).click();
    const dialog = page.getByRole("alertdialog", { name: RUNNERS.MANAGED_CONFIRM_TITLE });
    await dialog.getByRole("button", { name: RUNNERS.MANAGED_CONFIRM }).click();
    await expect(dialog.getByText(RUNNERS.MANAGED_REFUSED_TITLE)).toBeVisible();
    await expect(dialog.getByText(RUNNERS.MANAGED_NOT_SUPER)).toBeVisible();
    // The open dialog hides the page from the accessibility tree, so the switch is read once it closes.
    await dialog.getByRole("button", { name: RUNNER.CANCEL, exact: true }).click();
    await expect(dialog).toHaveCount(0);
    await expect(card.getByRole("switch", { name: RUNNERS.MANAGED_LABEL })).toHaveAttribute("aria-checked", "false");
  });

  test("a failed read says so, and Try again shows the switch", async ({ page }) => {
    await stubSettings(page, { enabled: true }, { getStatuses: [500] });
    const card = await openCard(page);
    await expect(card.getByText(RUNNERS.MANAGED_LOAD_FAILED)).toBeVisible();
    await card.getByRole("button", { name: RUNNERS.MANAGED_RETRY }).click();
    await expect(card.getByRole("switch", { name: RUNNERS.MANAGED_LABEL })).toHaveAttribute("aria-checked", "false");
  });
});
