/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, sql } from "./fixtures";
import type { Locator, Page } from "@playwright/test";
import { DESKTOP_FILES as C } from "../src/app/components/wardyn/copy/desktop-files";

// ============================================================================
// The run's Files panel (#2967, mock M7-Files): what the panel DRAWS from the three endpoints it
// calls. Every one is STUBBED in this spec with page.route (nothing here reaches the real endpoint):
//   GET /api/v1/runs/{id}/desktop/files          the rules and (download allowed only) the listing
//   PUT /api/v1/runs/{id}/desktop/files/{name}   an upload
//   GET /api/v1/runs/{id}/desktop/files/{name}   a download
// The seeded e2e backend cannot answer them for real. Its RUNNING run has no sandbox_ref (e2e-backend.sh
// sets one only under WARDYN_E2E_TMUX=1) and its runner is nil, so desktopRun (internal/api/
// desktop_files.go) correctly refuses with 409 not-RUNNING / 503 no-runner. The server half is
// pinned by the Go tests in internal/api/desktop_files_test.go (TestDesktopFiles_DefaultsUploadAllowed
// DownloadBlocked, _DownloadFollowsThePolicy, _RefusalsBeforeTheSandbox, _SponsorIsRefusedOnEvery
// Endpoint, _OnlyWhoMayEnterTheDesktopReachesIt, _UnreadableRulesRefuseEveryTransfer,
// _OneReadDecidesTheTransfer); the empty-SandboxRef refusal at desktop_files.go:101 is pinned on all three
// endpoints by _NoSandboxRefRefusesEveryEndpoint. The panel's own states are also covered by desktop-files-panel.test.tsx.
// ============================================================================

const RUN_TASK = "e2e fixture 2";

type Rule = "allowed" | "blocked";
interface Stub {
  rules?: Partial<{ paste_in: Rule; copy_out: Rule; upload: Rule; download: Rule }>;
  files?: { name: string; bytes: number }[];
}

function runId(): string {
  const id = sql(`SELECT id FROM agent_runs WHERE task = '${RUN_TASK}' LIMIT 1`);
  expect(id, `no seeded run with task ${RUN_TASK}`).toMatch(/^[0-9a-f-]{36}$/);
  return id;
}

// stubFiles answers the panel's endpoints the way desktop_files.go does for a healthy run: the
// defaults (paste in allowed, copy out blocked, upload allowed, download blocked), the folder
// listed only when download is allowed.
async function stubFiles(page: Page, id: string, { rules = {}, files = [] }: Stub = {}): Promise<void> {
  const effective = { paste_in: "allowed", copy_out: "blocked", upload: "allowed", download: "blocked", ...rules };
  await page.route(new RegExp(`/api/v1/runs/${id}/desktop/files(/[^/?]*)?(\\?.*)?$`), async (route) => {
    const req = route.request();
    const name = decodeURIComponent(new URL(req.url()).pathname.split("/desktop/files/")[1] ?? "");
    const json = (status: number, body: unknown) =>
      route.fulfill({ status, contentType: "application/json", body: JSON.stringify(body) });
    if (!name && req.method() === "GET") {
      return json(200, { folder: "~/Transfers", rules: effective, files: effective.download === "allowed" ? files : [] });
    }
    if (req.method() === "PUT") {
      if (name === "disk-image.iso") {
        return json(413, { error: "the file is over the 256 MiB limit for a desktop upload. Nothing was sent.", reason: "desktop_transfer_too_large" });
      }
      return json(200, { name, bytes: 5, sha256: "c3a1" + "0".repeat(56) + "0e9d" });
    }
    if (req.method() === "GET") {
      return route.fulfill({ status: 200, contentType: "application/octet-stream", body: "refund,total\n" });
    }
    return route.fallback();
  });
}

async function openPanel(page: Page, stub?: Stub): Promise<Locator> {
  const id = runId();
  await stubFiles(page, id, stub);
  await page.goto(`/runs/${id}`);
  await expect(page.getByRole("heading", { name: RUN_TASK, level: 1 })).toBeVisible();
  const panel = page.getByTestId("desktop-files");
  await panel.scrollIntoViewIfNeeded();
  await expect(panel).toBeVisible();
  return panel;
}

test.describe("Run detail — Files panel", () => {
  test("the defaults: the clipboard strip, an upload drop zone, and a blocked download that lists nothing", async ({ page }) => {
    const panel = await openPanel(page);
    await expect(panel.getByText(C.pasteIn, { exact: true }).locator("xpath=following-sibling::dd")).toHaveText(C.allowed);
    await expect(panel.getByText(C.copyOut, { exact: true }).locator("xpath=following-sibling::dd")).toHaveText(C.blocked);
    await expect(panel.getByText("~/Transfers", { exact: true })).toBeVisible();
    await expect(panel.getByText(C.drop, { exact: true })).toBeVisible();
    await expect(panel.getByText(C.dropSub, { exact: true })).toBeVisible();
    await expect(panel.getByRole("button", { name: C.choose })).toBeVisible();
    await expect(panel.getByText(C.everyFile, { exact: true })).toBeVisible();

    await panel.getByRole("tab", { name: C.fromTab }).click();
    await expect(panel.getByText(C.downloadBlocked, { exact: true })).toBeVisible();
    await expect(panel.getByText(C.refused, { exact: true })).toBeVisible();
    await expect(panel.getByText(C.blockedEmpty, { exact: true })).toBeVisible();
    await expect(panel.getByText(C.refusedLogged, { exact: true })).toBeVisible();
    await expect(panel.getByTestId("desktop-download")).toHaveCount(0);
  });

  test("with upload blocked the drop zone is replaced by the refusal", async ({ page }) => {
    const panel = await openPanel(page, { rules: { upload: "blocked" } });
    await expect(panel.getByText(C.uploadBlocked, { exact: true })).toBeVisible();
    await expect(panel.getByText(C.refused, { exact: true })).toBeVisible();
    await expect(panel.getByText(C.drop, { exact: true })).toHaveCount(0);
    await expect(panel.getByRole("button", { name: C.choose })).toHaveCount(0);
  });

  test("an upload is delivered with its hash, and one over the limit is not sent", async ({ page }) => {
    const panel = await openPanel(page);
    await panel.getByLabel(C.choose).setInputFiles([
      { name: "notes.csv", mimeType: "text/csv", buffer: Buffer.from("a,b,c") },
      { name: "disk-image.iso", mimeType: "application/octet-stream", buffer: Buffer.from("x") },
    ]);
    const rows = panel.getByTestId("desktop-upload");
    await expect(rows).toHaveCount(2);
    await expect(rows.nth(0)).toContainText("notes.csv");
    await expect(rows.nth(0)).toContainText(C.delivered);
    await expect(rows.nth(0)).toContainText(/sha256 c3a1…0e9d · \d\d:\d\d/);
    await expect(rows.nth(1)).toContainText(C.tooLarge);
    await expect(rows.nth(1)).toContainText(C.tooLargeSub);
  });

  test("with download allowed the folder is listed and a download reports its hash", async ({ page }) => {
    const panel = await openPanel(page, { rules: { download: "allowed" }, files: [{ name: "refund-report.csv", bytes: 12 * 1024 }] });
    await panel.getByRole("tab", { name: C.fromTab }).click();
    const row = panel.getByTestId("desktop-download");
    await expect(row).toHaveCount(1);
    await expect(row).toContainText("refund-report.csv");
    await expect(row).toContainText("12 KB");
    await expect(panel.getByText(C.downloadsAllowed, { exact: true })).toBeVisible();
    const download = page.waitForEvent("download");
    await row.getByRole("button", { name: `${C.download} refund-report.csv` }).click();
    expect((await download).suggestedFilename()).toBe("refund-report.csv");
    await expect(row).toContainText(/Downloaded \d\d:\d\d · sha256 [0-9a-f]{4}…[0-9a-f]{4}/);
  });
});
