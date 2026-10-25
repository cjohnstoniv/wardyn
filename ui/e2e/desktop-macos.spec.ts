/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { Page } from "@playwright/test";
import { test, expect, sql, gotoConsole, navToRoute } from "./fixtures";
import { NR_MACHINE } from "../src/app/components/wardyn/copy/new-run/machine";
import { NR_SHAPE } from "../src/app/components/wardyn/copy/new-run/shape";
import { NR_START } from "../src/app/components/wardyn/copy/new-run/start";
import { DESKTOP } from "../src/app/components/wardyn/copy/desktop";

// ============================================================================
// A macOS VM (mocks M-N1, M-N2, M-D1): the New Run OS row with the organisation switch on, the changes panel
// for macOS, and the run page's Desktop block for a macOS run. The `none` runner behind this backend has no
// Mac, so the setup status carries the switch and the seeded RUNNING run is shown as a macOS VM on the wire;
// the Mac's offer, base phases and disk removal are the runner's words and are pinned in the unit tests.
// ============================================================================

const RUN_TASK = "e2e fixture 2";

function runIdByTask(task: string): string {
  const id = sql(`SELECT id FROM agent_runs WHERE task = '${task}' LIMIT 1`);
  expect(id, `no seeded run with task ${task}`).toMatch(/^[0-9a-f-]{36}$/);
  return id;
}

/** The real setup status with WARDYN_MACOS_VMS on or off. */
async function macosSwitch(page: Page, on: boolean) {
  await page.route("**/api/v1/setup/status*", async (route) => {
    const body = (await (await route.fetch()).json()) as { features?: Record<string, unknown> };
    await route.fulfill({ json: { ...body, features: { ...body.features, macos_vms: on } } });
  });
}

async function openVMStep(page: Page) {
  await gotoConsole(page);
  await navToRoute(page, "/runs/new");
  await page.getByRole("button", { name: NR_START.GUIDED_FOOT }).click();
  await page.getByRole("button", { name: NR_SHAPE.ENV_TITLE, exact: true }).click();
  await page.getByRole("group", { name: NR_MACHINE.LABEL }).getByRole("button", { name: NR_MACHINE.VM }).click();
  return page.getByRole("group", { name: NR_MACHINE.OS_LABEL });
}

test.describe("New Run: the macOS option", () => {
  test("with the organisation switch off the OS row is Linux alone, with the macOS and Windows line", async ({ page }) => {
    await macosSwitch(page, false);
    const os = await openVMStep(page);
    await expect(os.getByRole("button", { name: NR_MACHINE.OS_LINUX })).toHaveAttribute("aria-pressed", "true");
    await expect(os.getByRole("button", { name: NR_MACHINE.OS_MACOS })).toHaveCount(0);
    await expect(os.getByText(NR_MACHINE.OS_LATER)).toBeVisible();
  });

  test("with it on, macOS joins Linux, and choosing it swaps the changes panel's rows for the macOS ones", async ({ page }) => {
    await macosSwitch(page, true);
    const os = await openVMStep(page);
    await expect(os.getByText(NR_MACHINE.OS_WINDOWS_LATER)).toBeVisible();
    const changes = page.getByRole("region", { name: NR_MACHINE.CHANGES_TITLE });
    await expect(changes.getByText(NR_MACHINE.CHANGES_SIGN_INS)).toHaveCount(0);

    await os.getByRole("button", { name: NR_MACHINE.OS_MACOS }).click();
    await expect(os.getByRole("button", { name: NR_MACHINE.OS_MACOS })).toHaveAttribute("aria-pressed", "true");
    await expect(os.getByText(NR_MACHINE.OS_WINDOWS_LATER)).toHaveCount(0);
    await expect(page.getByTestId("nr-vm-barrier")).toContainText(NR_MACHINE.BARRIER_VM);
    await expect(changes.getByText(NR_MACHINE.CHANGES_CLIPBOARD_MACOS_NONE)).toBeVisible();
    await expect(changes.getByText(NR_MACHINE.CHANGES_SIGN_INS_RUN_DISK)).toBeVisible();
    await expect(changes.getByText(NR_MACHINE.CHANGES_MAC_MANAGED)).toBeVisible();

    await os.getByRole("button", { name: NR_MACHINE.OS_LINUX }).click();
    await expect(changes.getByText(NR_MACHINE.CHANGES_CLIPBOARD_BODY)).toBeVisible();
    await expect(changes.getByText(NR_MACHINE.CHANGES_MAC_MANAGED)).toHaveCount(0);
  });
});

test.describe("Run detail: a macOS desktop", () => {
  test("the Desktop block names the macOS kind and keeps its sign-ins on the run's disk", async ({ page }) => {
    const id = runIdByTask(RUN_TASK);
    await page.route(`**/api/v1/runs/${id}`, async (route) => {
      const res = await route.fetch();
      await route.fulfill({ response: res, body: JSON.stringify({ ...(await res.json()), machine_kind: "macos_vm" }) });
    });
    await page.route(`**/api/v1/runs/${id}/desktop/sign-ins`, (route) =>
      route.fulfill({ json: { store: "run_disk", paused: false, can_pause: true, agent: "Claude Code" } }),
    );
    await gotoConsole(page);
    await page.goto(`/runs/${id}`);
    await expect(page.getByRole("heading", { name: RUN_TASK, level: 1 })).toBeVisible();
    const desk = page.getByRole("region", { name: DESKTOP.title });
    await desk.scrollIntoViewIfNeeded();
    await expect(desk.getByText(DESKTOP.kindMacOS)).toBeVisible();
    await expect(desk.getByRole("button", { name: DESKTOP.pauseToSignIn })).toBeVisible();
    await expect(desk.getByText(DESKTOP.signInsNoteRunDisk, { exact: true })).toBeVisible();
    await expect(desk.getByText(DESKTOP.signInsNote, { exact: true })).toHaveCount(0);
  });
});
