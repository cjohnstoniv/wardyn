/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { Page } from "@playwright/test";
import { test, expect, sql, gotoConsole } from "./fixtures";
import { DESKTOP } from "../src/app/components/wardyn/copy/desktop";
import { aheadByHours } from "../src/app/lib/test-clock";

// ============================================================================
// A finished macOS VM run's disk line (mock M-D1 frames 9 to 11, MV-D10/D11/D12): GET /runs/{id}'s macos_disk
// onto the Desktop block of "Attach from your terminal". The `none` runner behind this backend has no Mac, so
// the COMPLETED seeded run is patched into a macOS VM on the wire; the server's own side is pinned in the Go
// tests (run_macos_disk_test.go, run_macos_disk_pg_test.go).
// ============================================================================

const RUN_TASK = "e2e fixture 4";
const AT = aheadByHours(-1);
const FAILED_PREFIX = "The macOS VM's disk could not be removed";

function runIdByTask(task: string): string {
  const id = sql(`SELECT id FROM agent_runs WHERE task = '${task}' LIMIT 1`);
  expect(id, `no seeded run with task ${task}`).toMatch(/^[0-9a-f-]{36}$/);
  return id;
}

/** Answers GET /runs/{id} with the real body plus what over() says now (read on every poll), dropping macos_disk when it is absent. */
async function patchRun(page: Page, id: string, over: () => Record<string, unknown>) {
  await page.route(`**/api/v1/runs/${id}`, async (route) => {
    const res = await route.fetch();
    const body = { ...(await res.json()), ...over() } as Record<string, unknown>;
    if (body.macos_disk === undefined) delete body.macos_disk;
    await route.fulfill({ response: res, body: JSON.stringify(body) });
  });
  await page.route(`**/api/v1/runs/${id}/desktop/sign-ins`, (route) =>
    route.fulfill({ json: { store: "run_disk", paused: false, can_pause: false, agent: "Claude Code" } }),
  );
}

async function openRun(page: Page, id: string) {
  await gotoConsole(page);
  await page.goto(`/runs/${id}`);
  await expect(page.getByRole("heading", { name: RUN_TASK, level: 1 })).toBeVisible();
}

const desktop = (page: Page) => page.getByRole("region", { name: DESKTOP.title });
const localTime = (page: Page, iso: string) =>
  page.evaluate((s) => new Date(s).toLocaleTimeString(undefined, { hour: "numeric", minute: "2-digit" }), iso);

test.describe("Run detail: a finished macOS VM run's disk", () => {
  test("frame 9: removing, with the desktop Off and no buttons", async ({ page }) => {
    const id = runIdByTask(RUN_TASK);
    await patchRun(page, id, () => ({ machine_kind: "macos_vm", macos_disk: { state: "removing" } }));
    await openRun(page, id);
    const desk = desktop(page);
    await desk.scrollIntoViewIfNeeded();
    await expect(desk.getByText(DESKTOP.diskRemoving)).toBeVisible();
    await expect(desk.getByText(DESKTOP.statusOff)).toBeVisible();
    await expect(desk.getByRole("button")).toHaveCount(0);
  });

  test("frame 10: removed, at the browser's local time", async ({ page }) => {
    const id = runIdByTask(RUN_TASK);
    await patchRun(page, id, () => ({ machine_kind: "macos_vm", macos_disk: { state: "removed", at: AT } }));
    await openRun(page, id);
    await expect(desktop(page).getByText(DESKTOP.diskRemoved(await localTime(page, AT)))).toBeVisible();
  });

  test("frame 11: kept because the helper is alive, as an alert naming the files in use", async ({ page }) => {
    const id = runIdByTask(RUN_TASK);
    await patchRun(page, id, () => ({ machine_kind: "macos_vm", macos_disk: { state: "kept", reason: "helper_alive" } }));
    await openRun(page, id);
    await expect(desktop(page).getByRole("alert")).toHaveText(DESKTOP.diskRemoveFailed(DESKTOP.diskReasonInUse));
  });

  test("a reason code the console doesn't know says the Mac didn't say why", async ({ page }) => {
    const id = runIdByTask(RUN_TASK);
    await patchRun(page, id, () => ({ machine_kind: "macos_vm", macos_disk: { state: "kept", reason: "future_code" } }));
    await openRun(page, id);
    await expect(desktop(page).getByRole("alert")).toHaveText(DESKTOP.diskRemoveFailed(DESKTOP.diskReasonUnknown));
  });

  test("removing turns into removed on the page's own poll, with no reload", async ({ page }) => {
    const id = runIdByTask(RUN_TASK);
    let removed = false;
    await patchRun(page, id, () => ({
      machine_kind: "macos_vm",
      macos_disk: removed ? { state: "removed", at: AT } : { state: "removing" },
    }));
    await openRun(page, id);
    await expect(desktop(page).getByText(DESKTOP.diskRemoving)).toBeVisible();
    removed = true;
    await expect(desktop(page).getByText(DESKTOP.diskRemoved(await localTime(page, AT)))).toBeVisible({ timeout: 15_000 });
  });

  test("a finished Linux VM run never shows the disk line", async ({ page }) => {
    const id = runIdByTask(RUN_TASK);
    await patchRun(page, id, () => ({ machine_kind: "linux_vm", macos_disk: { state: "removed", at: AT } }));
    await openRun(page, id);
    await expect(desktop(page)).toHaveCount(0);
  });

  test("the real server sends no macos_disk for a container run, and the page shows no terminal card", async ({ page }) => {
    const id = runIdByTask(RUN_TASK);
    const body = page.waitForResponse((r) => new URL(r.url()).pathname === `/api/v1/runs/${id}` && r.request().method() === "GET");
    await openRun(page, id);
    const json = (await (await body).json()) as Record<string, unknown>;
    expect(json.machine_kind ?? "").toBe("");
    expect(json).not.toHaveProperty("macos_disk");
    await expect(desktop(page)).toHaveCount(0);
    await expect(page.getByText("Attach from your terminal")).toHaveCount(0);
  });

  test("a runner too old to report: removing, then no disk line on the poll, and never the failure line", async ({ page }) => {
    const id = runIdByTask(RUN_TASK);
    let windowOver = false;
    await patchRun(page, id, () => ({ machine_kind: "macos_vm", macos_disk: windowOver ? undefined : { state: "removing" } }));
    await openRun(page, id);
    await expect(desktop(page).getByText(DESKTOP.diskRemoving)).toBeVisible();
    await expect(page.getByText(FAILED_PREFIX, { exact: false })).toHaveCount(0);
    windowOver = true;
    await expect(desktop(page)).toHaveCount(0, { timeout: 15_000 });
    await expect(page.getByText(FAILED_PREFIX, { exact: false })).toHaveCount(0);
  });

  test("a kept disk a later sweep removed shows removed on the next load", async ({ page }) => {
    const id = runIdByTask(RUN_TASK);
    let swept = false;
    await patchRun(page, id, () => ({
      machine_kind: "macos_vm",
      macos_disk: swept ? { state: "removed", at: AT } : { state: "kept", reason: "remove_failed" },
    }));
    await openRun(page, id);
    await expect(desktop(page).getByRole("alert")).toHaveText(DESKTOP.diskRemoveFailed(DESKTOP.diskReasonDeleteFailed));
    swept = true;
    await page.reload();
    await expect(page.getByRole("heading", { name: RUN_TASK, level: 1 })).toBeVisible();
    await expect(desktop(page).getByText(DESKTOP.diskRemoved(await localTime(page, AT)))).toBeVisible();
    await expect(page.getByText(FAILED_PREFIX, { exact: false })).toHaveCount(0);
  });
});
