/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { Page } from "@playwright/test";
import { test, expect, gotoConsole } from "./fixtures";
import { MACVM_K, MACVM_P, MACVM_R } from "../src/app/components/wardyn/copy/runner-macvm";
import { DESKTOP } from "../src/app/components/wardyn/copy/desktop";
import { RES } from "../src/app/components/wardyn/copy/runner-pools";
import { aheadByHours } from "../src/app/lib/test-clock";

// Admin -> Runners, a Mac's macOS virtual machines (mocks M-R1 drawer section, M-R2 card badge). The harness has
// no registered runner, so the runner list is answered here with Macs in each base state and one runner that
// sends no macOS VM caps; what is pinned is that the console draws the section and badge from those fields.
const none = { runs: 0, cpu_millis: 0, memory_mib: 0, disk_mib: 0 };
function mac(name: string, id: string, macos_vm?: Record<string, unknown>, active = 0) {
  return {
    id, name, state: "claimed", online: true, key_fingerprint: "SHA256:fp", key_fingerprint_abbreviated: false,
    created_at: aheadByHours(-7 * 24), claimed_at: aheadByHours(-7 * 24 + 1), kind: "self_hosted",
    owner: "dana@example.com", owner_kind: "person", trust_class: "managed_device",
    posture: { mdm_managed: true, disk_encrypted: true, os: "darwin", os_version: "27.0.1", evidence: "mdm_file" },
    limits: { runs_at_once: 2, total: { cpu_millis: 8000, memory_mib: 20480, disk_mib: 0 }, per_run: { cpu_millis: 4000, memory_mib: 8192, disk_mib: 0 } },
    usage: { online: true, in_use: { ...none, runs: active, macos_vm_runs: active || undefined }, available: none, disk_capped: true },
    pools: [], macos_vm,
  };
}
const READY = {
  max: 2, min_cpus: 4, min_memory_mib: 8192, harnesses: ["claude-code", "codex-cli", "none"], desktop: "control",
  base: { state: "ready", guest_os: "macOS 27.0.1 (26A434)" },
};
const RUNNERS = [
  mac("mac-ready", "00000000-0000-4000-8000-0000000000c1", READY, 1),
  mac("mac-preparing", "00000000-0000-4000-8000-0000000000c2", { max: 2, base: { state: "preparing", progress: 42 } }),
  mac("mac-failed", "00000000-0000-4000-8000-0000000000c3", { max: 0, base: { state: "failed", error: "connection reset by peer" } }),
  mac("linux-box", "00000000-0000-4000-8000-0000000000c4"),
  // A base 41 days old: past the 30-day maximum the too-old test sets.
  mac("mac-old", "00000000-0000-4000-8000-0000000000c5", {
    ...READY,
    base: { ...READY.base, created_at: aheadByHours(-41 * 24), guest: { os_version: "27.0.1", sip: "on", gatekeeper: "on", profiles: 2, mdm: "not_enrolled", org_ca: "trusted" } },
  }),
];

async function openRunners(page: Page) {
  await page.route("**/api/v1/runners/settings", (route) => route.fulfill({ json: { enabled: true } }));
  await page.route("**/api/v1/runners/tokens**", (route) => route.fulfill({ json: [] }));
  await page.route("**/api/v1/remote-runners", (route) => route.fulfill({ json: [] }));
  await page.route(/\/api\/v1\/runners\?/, (route) => route.fulfill({ json: RUNNERS }));
  await gotoConsole(page, "admin");
  await page.goto("/admin/runners");
}
const card = (page: Page, name: string) => page.getByRole("listitem", { name, exact: true });
async function openDrawer(page: Page, name: string) {
  await card(page, name).getByRole("button", { name, exact: true }).click();
  return page.getByRole("dialog");
}

test.describe("Runner cards: the macOS VMs badge", () => {
  test("shows the count, preparing with its percent, and not offered, and none on a runner without caps", async ({ page }) => {
    await openRunners(page);
    await expect(card(page, "mac-ready").getByText(MACVM_K.K01("1", "2"))).toBeVisible();
    await expect(card(page, "mac-preparing").getByText(MACVM_K.K02("42"))).toBeVisible();
    await expect(card(page, "mac-failed").getByText(MACVM_K.K03)).toBeVisible();
    await expect(card(page, "linux-box").getByText(/macOS VMs/)).toHaveCount(0);
  });
});

test.describe("Runner drawer: macOS virtual machines", () => {
  test("ready: the guest, the live runs, the smallest run and the desktop", async ({ page }) => {
    await openRunners(page);
    const drawer = await openDrawer(page, "mac-ready");
    const section = drawer.locator("[data-macvm]");
    await expect(section.getByRole("heading", { name: MACVM_R.R00 })).toBeVisible();
    await expect(section.getByText(DESKTOP.ready, { exact: true })).toBeVisible();
    await expect(section.getByText(MACVM_R.R06("macOS 27.0.1 (26A434)"))).toBeVisible();
    await expect(section.getByText(RES.RUNNING_NOW(1, 2))).toBeVisible();
    await expect(section.getByText(`${RES.CPUS(4)} · ${RES.GB(8)} memory`)).toBeVisible();
    await expect(section.getByText(MACVM_R.R24)).toBeVisible();
    await expect(section.getByText(MACVM_R.R25)).toBeVisible();
  });

  test("preparing: the chip and its percent", async ({ page }) => {
    await openRunners(page);
    const section = (await openDrawer(page, "mac-preparing")).locator("[data-macvm]");
    await expect(section.getByText(MACVM_R.R10, { exact: true })).toBeVisible();
    await expect(section.getByRole("img", { name: "42%" })).toBeVisible();
  });

  test("failed: the runner's own error", async ({ page }) => {
    await openRunners(page);
    const section = (await openDrawer(page, "mac-failed")).locator("[data-macvm]");
    await expect(section.getByText(MACVM_R.R15, { exact: true })).toBeVisible();
    await expect(section.getByText("connection reset by peer")).toBeVisible();
  });

  test("a base older than the organisation's maximum is not offered, with its age and guest posture", async ({ page }) => {
    await page.route("**/api/v1/site-config", (route) => route.fulfill({ json: { macos_vm: { max_base_age_days: 30 } } }));
    await openRunners(page);
    const section = (await openDrawer(page, "mac-old")).locator("[data-macvm]");
    await expect(section.getByText(MACVM_P.P12, { exact: true })).toBeVisible();
    await expect(section.getByText(MACVM_R.R14, { exact: true })).toBeVisible();
    await expect(section.getByText(MACVM_P.P01, { exact: true })).toBeVisible();
    await expect(section.getByText(MACVM_P.P07_INSTALLED(2))).toBeVisible();
    await expect(section.getByText(MACVM_R.R25)).toHaveCount(0);
  });

  test("a runner that sends no macOS VM caps has no section", async ({ page }) => {
    await openRunners(page);
    const drawer = await openDrawer(page, "linux-box");
    await expect(drawer.getByRole("heading", { name: "linux-box" })).toBeVisible();
    await expect(drawer.locator("[data-macvm]")).toHaveCount(0);
  });
});
