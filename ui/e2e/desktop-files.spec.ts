/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, sql } from "./fixtures";
import type { Locator, Page } from "@playwright/test";

// ============================================================================
// The run's Files panel (#2967, mock M7-Files) on the SEEDED backend. The seeded
// RUNNING run declares a UI app and no desktop rules, so GET /desktop/files is
// real here and answers the defaults: paste in allowed, copy out blocked, upload
// allowed, download blocked. The `none` runner has no sandbox to move files into,
// so the transfers themselves are fulfilled locally; the server half is pinned
// by internal/api desktop_files_test.go.
// ============================================================================

const RUN_TASK = "e2e fixture 2";
const EVERY_FILE = "Every file is logged by direction, size and hash. Never the content.";

function runId(): string {
  const id = sql(`SELECT id FROM agent_runs WHERE task = '${RUN_TASK}' LIMIT 1`);
  expect(id, `no seeded run with task ${RUN_TASK}`).toMatch(/^[0-9a-f-]{36}$/);
  return id;
}

async function openPanel(page: Page): Promise<Locator> {
  await page.goto(`/runs/${runId()}`);
  await expect(page.getByRole("heading", { name: RUN_TASK, level: 1 })).toBeVisible();
  const panel = page.getByTestId("desktop-files");
  await panel.scrollIntoViewIfNeeded();
  await expect(panel).toBeVisible();
  return panel;
}

test.describe("Run detail — Files panel", () => {
  test("the defaults: the clipboard strip, an upload drop zone, and a blocked download that lists nothing", async ({ page }) => {
    const panel = await openPanel(page);
    await expect(panel.getByText("Paste in", { exact: true }).locator("xpath=following-sibling::dd")).toHaveText("Allowed");
    await expect(panel.getByText("Copy out", { exact: true }).locator("xpath=following-sibling::dd")).toHaveText("Blocked");
    await expect(panel.getByText("~/Transfers", { exact: true })).toBeVisible();
    await expect(panel.getByText("Drop files here or choose files", { exact: true })).toBeVisible();
    await expect(panel.getByText("They land in the transfer folder.", { exact: true })).toBeVisible();
    await expect(panel.getByRole("button", { name: "Choose files" })).toBeVisible();
    await expect(panel.getByText(EVERY_FILE, { exact: true })).toBeVisible();

    await panel.getByRole("tab", { name: "From the desktop" }).click();
    await expect(panel.getByText("Blocked: downloading files", { exact: true })).toBeVisible();
    await expect(panel.getByText("Refused by policy", { exact: true })).toBeVisible();
    await expect(
      panel.getByText("Files the agent saves in the transfer folder stay in the desktop. Nothing is listed here.", { exact: true }),
    ).toBeVisible();
    await expect(panel.getByText("Refused attempts are logged by direction and size too.", { exact: true })).toBeVisible();
    await expect(panel.getByTestId("desktop-download")).toHaveCount(0);
  });

  test("an upload is delivered with its hash, and one over the limit is not sent", async ({ page }) => {
    const id = runId();
    await page.route(`**/api/v1/runs/${id}/desktop/files/*`, async (route) => {
      const name = decodeURIComponent(new URL(route.request().url()).pathname.split("/").pop() ?? "");
      if (route.request().method() !== "PUT") return route.fallback();
      if (name === "disk-image.iso") {
        return route.fulfill({
          status: 413,
          contentType: "application/json",
          body: JSON.stringify({ error: "the file is over the 256 MiB limit for a desktop upload. Nothing was sent.", reason: "desktop_transfer_too_large" }),
        });
      }
      return route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ name, bytes: 5, sha256: "c3a1" + "0".repeat(56) + "0e9d" }) });
    });
    const panel = await openPanel(page);
    await panel.getByLabel("Choose files").setInputFiles([
      { name: "notes.csv", mimeType: "text/csv", buffer: Buffer.from("a,b,c") },
      { name: "disk-image.iso", mimeType: "application/octet-stream", buffer: Buffer.from("x") },
    ]);
    const rows = panel.getByTestId("desktop-upload");
    await expect(rows).toHaveCount(2);
    await expect(rows.nth(0)).toContainText("notes.csv");
    await expect(rows.nth(0)).toContainText("Delivered");
    await expect(rows.nth(0)).toContainText(/sha256 c3a1…0e9d · \d\d:\d\d/);
    await expect(rows.nth(1)).toContainText("Too large");
    await expect(rows.nth(1)).toContainText("Over the size limit for this desktop. Nothing was sent.");
  });

  test("with download allowed the folder is listed and a download reports its hash", async ({ page }) => {
    const id = runId();
    await page.route(`**/api/v1/runs/${id}/desktop/files`, async (route) => {
      const res = await route.fetch();
      const body = await res.json();
      body.rules.download = "allowed";
      body.files = [{ name: "refund-report.csv", bytes: 12 * 1024 }];
      await route.fulfill({ response: res, body: JSON.stringify(body) });
    });
    await page.route(`**/api/v1/runs/${id}/desktop/files/refund-report.csv`, (route) =>
      route.fulfill({ status: 200, contentType: "application/octet-stream", body: "refund,total\n" }),
    );
    const panel = await openPanel(page);
    await panel.getByRole("tab", { name: "From the desktop" }).click();
    const row = panel.getByTestId("desktop-download");
    await expect(row).toHaveCount(1);
    await expect(row).toContainText("refund-report.csv");
    await expect(row).toContainText("12 KB");
    await expect(panel.getByText("Downloads were allowed by your team. Each one is logged by size and hash.", { exact: true })).toBeVisible();
    const download = page.waitForEvent("download");
    await row.getByRole("button", { name: "Download refund-report.csv" }).click();
    expect((await download).suggestedFilename()).toBe("refund-report.csv");
    await expect(row).toContainText(/Downloaded \d\d:\d\d · sha256 [0-9a-f]{4}…[0-9a-f]{4}/);
  });
});
