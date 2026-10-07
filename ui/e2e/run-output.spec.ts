/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Run-detail Output tab (0.8.6 out-o5, mock packet M8). The seeded `none`
// runner never keeps real output, so GET /runs/{id}/output is spliced with a
// fixed body (a headless run's output, an interactive run's pane snapshot);
// the real run, route and tab wiring are the harness's own.
import { randomUUID } from "node:crypto";
import type { Page } from "@playwright/test";
import { test, expect, consoleAPI, gotoConsole, navToRoute } from "./fixtures";
import { RUN_OUTPUT } from "../src/app/components/wardyn/copy";

const OUTPUT_GLOB = "**/api/v1/runs/*/output";
const POLL_MS = 4000;

const body = (o: Record<string, unknown>) => ({
  output: "$ go test ./...\nok   example.com/app   4.118s\n<script>alert(1)</script>",
  truncated: false,
  complete: true,
  source: "stdout",
  incomplete: false,
  capture_gap: false,
  mask_scope: "run",
  captured_at: "2026-10-03T14:02:00Z",
  ...o,
});

async function openOutputTab(page: Page): Promise<string> {
  await gotoConsole(page);
  const created = await consoleAPI(page, "POST", "/api/v1/runs", {
    agent: "claude-code",
    task: "run-output e2e " + randomUUID().slice(0, 8),
  });
  expect(created.status, created.text).toBe(201);
  const id = JSON.parse(created.text).id as string;
  await navToRoute(page, `/runs/${id}`);
  await page.getByRole("tab", { name: RUN_OUTPUT.tab }).click();
  return id;
}

test.describe("Run output tab", () => {
  test("a spliced headless run: the panel polls while live and stops once complete", async ({ page }) => {
    let calls = 0;
    await page.route(OUTPUT_GLOB, async (route) => {
      calls++;
      await route.fulfill({
        json: calls < 2 ? body({ complete: false, captured_at: undefined, output: "$ go test ./..." }) : body({}),
      });
    });
    const id = await openOutputTab(page);

    await expect(page.getByText(RUN_OUTPUT.sourceStdout)).toBeVisible();
    await expect(page.getByText(RUN_OUTPUT.final)).toBeVisible({ timeout: POLL_MS * 3 });
    const pre = page.getByTestId("run-output-text");
    // Sandbox markup shows as literal text, never as an element.
    await expect(pre).toContainText("<script>alert(1)</script>");
    await expect(pre.locator("script")).toHaveCount(0);
    await expect(page.getByText(`wardyn run output ${id}`)).toBeVisible();

    const settled = calls;
    await page.waitForTimeout(POLL_MS + 2000);
    expect(calls).toBe(settled);
  });

  test("a spliced pane snapshot: Last screen and its caption", async ({ page }) => {
    await page.route(OUTPUT_GLOB, (route) =>
      route.fulfill({ json: body({ source: "pane_snapshot", output: "user@sandbox:~$ " }) }),
    );
    await openOutputTab(page);
    await expect(page.getByText(RUN_OUTPUT.sourcePane, { exact: true })).toBeVisible();
    await expect(page.getByText(RUN_OUTPUT.paneCaption)).toBeVisible();
    await expect(page.getByTestId("run-output-text")).toContainText("user@sandbox:~$");
  });
});
