/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A run whose data_out rule keeps its content in, on the run page. The recording replay's refusals: the
// owner's replay is answered 403 data_out_blocked, or 503 data_out_unreadable, with the server's own
// sentence (the server side is pinned in Go, internal/api/data_out_test.go). The recording route is
// stubbed; the run is a seeded one.
import { randomUUID } from "node:crypto";
import type { Page, Route } from "@playwright/test";
import { test, expect, consoleAPI, gotoConsole, navToRoute, sql } from "./fixtures";
import { attachModeFrame, findRunningFixture, stubAttachSocket, stubAttachTicket, stubInteractiveRun } from "./attach-stub";
import { DESKTOP_FILES as C } from "../src/app/components/wardyn/copy/desktop-files";
import { DATA_OUT_RUN } from "../src/app/components/wardyn/copy/data-out-run";

const BLOCKED = "This run's data_out rule keeps its content in: only a super admin can replay its recording.";
const UNREADABLE = "This run's data_out rule could not be read. Try again.";

async function seedRun(page: Page): Promise<string> {
  const created = await consoleAPI(page, "POST", "/api/v1/runs", {
    agent: "claude-code",
    task: "data-out run e2e " + randomUUID().slice(0, 8),
  });
  expect(created.status, created.text).toBe(201);
  return JSON.parse(created.text).id as string;
}

// Answers the run's own recording read with status and a refusal body, and counts the asks.
async function refuseRecording(page: Page, id: string, status: number, reason: string, error: string): Promise<{ asks: () => number }> {
  let asks = 0;
  await page.route(new RegExp(`/api/v1/runs/${id}/recording/${id}$`), async (route: Route) => {
    asks++;
    await route.fulfill({ status, json: { error, reason } });
  });
  return { asks: () => asks };
}

async function openRecordingTab(page: Page, id: string): Promise<void> {
  await navToRoute(page, `/runs/${id}`);
  await page.getByRole("tab", { name: "Recording" }).click();
  await expect(page.getByRole("tab", { name: "Recording" })).toHaveAttribute("aria-selected", "true");
}

// The run's recorded policy, as GET /runs/{id}/policy answers it: the one read the terminal line and the Files
// lines take the rule from.
async function stubPolicy(page: Page, id: string, dataOut: "allowed" | "review" | "blocked"): Promise<void> {
  await page.route(`**/api/v1/runs/${id}/policy`, (route) =>
    route.fulfill({
      json: { run_id: id, state: "recorded", source: { kind: "default" }, spec: { data_out: dataOut }, redacted: false, changes: [] },
    }),
  );
}

// The Files panel's own read, stubbed as desktop-files.spec.ts does: the seeded backend has no sandbox to list.
async function stubFiles(page: Page, id: string, download: "blocked" | "review"): Promise<void> {
  await page.route(new RegExp(`/api/v1/runs/${id}/desktop/files(\\?.*)?$`), (route) =>
    route.fulfill({
      json: {
        folder: "~/Transfers",
        rules: { paste_in: "allowed", copy_out: "blocked", upload: "allowed", download },
        files: download === "review" ? [{ name: "results.parquet", bytes: 6 * 1024 * 1024 }] : [],
      },
    }),
  );
  await page.route(/\/api\/v1\/approvals(\?.*)?$/, (route) => route.fulfill({ json: [] }));
}

// The seeded RUNNING run with its declared apps taken off the read: a run with no desktop.
async function stubNoApps(page: Page, id: string): Promise<void> {
  let cached: Record<string, unknown> | null = null;
  await page.route(`**/api/v1/runs/${id}`, async (route) => {
    if (route.request().method() !== "GET") return route.fallback();
    if (!cached) {
      const json = await (await route.fetch()).json();
      json.ui_apps = [];
      cached = json;
    }
    await route.fulfill({ json: cached! });
  });
}

const seededRunId = (): string => {
  const id = sql("SELECT id FROM agent_runs WHERE task = 'e2e fixture 2' LIMIT 1");
  expect(id, "no seeded run with task e2e fixture 2").toMatch(/^[0-9a-f-]{36}$/);
  return id;
};

test.describe("Run page: data_out", () => {
  test("a held rule says the recording stays in, with the server's sentence", async ({ page }) => {
    await gotoConsole(page, "admin");
    const id = await seedRun(page);
    await refuseRecording(page, id, 403, "data_out_blocked", BLOCKED);
    await openRecordingTab(page, id);
    const box = page.getByTestId("recording-data-out");
    await expect(box).toContainText("This run's recording stays in");
    await expect(box).toContainText(BLOCKED);
    await expect(page.getByText("No recording available")).toHaveCount(0);
  });

  test("an unreadable rule says so, and Try again asks again", async ({ page }) => {
    await gotoConsole(page, "admin");
    const id = await seedRun(page);
    const route = await refuseRecording(page, id, 503, "data_out_unreadable", UNREADABLE);
    await openRecordingTab(page, id);
    await expect(page.getByText(UNREADABLE)).toBeVisible();
    const before = route.asks();
    await page.getByRole("button", { name: "Try again" }).click();
    await expect.poll(route.asks).toBeGreaterThan(before);
  });
  test("a run that keeps its content in says its terminal does not copy out", async ({ page }) => {
    const { id } = await findRunningFixture(page);
    await stubInteractiveRun(page, id);
    await stubAttachTicket(page, id);
    await stubAttachSocket(page, (_n, ws) => ws.send(attachModeFrame(false)));
    await stubPolicy(page, id, "blocked");
    await gotoConsole(page);
    await navToRoute(page, `/runs/${id}`);
    await expect(page.getByTestId("run-terminal-pane").locator(".xterm-screen").first()).toBeVisible();
    await expect(page.getByTestId("terminal-keeps-in")).toHaveText(DATA_OUT_RUN.TERMINAL);
  });

  test("a run whose rule the page cannot read says its terminal does not copy out, and why", async ({ page }) => {
    const { id } = await findRunningFixture(page);
    await stubInteractiveRun(page, id);
    await stubAttachTicket(page, id);
    await stubAttachSocket(page, (_n, ws) => ws.send(attachModeFrame(false)));
    await page.route(`**/api/v1/runs/${id}/policy`, (route) => route.fulfill({ status: 503, json: { error: "boom" } }));
    await gotoConsole(page);
    await navToRoute(page, `/runs/${id}`);
    await expect(page.getByTestId("run-terminal-pane").locator(".xterm-screen").first()).toBeVisible();
    await expect(page.getByTestId("terminal-keeps-in")).toHaveText(DATA_OUT_RUN.TERMINAL_UNREAD);
  });

  test("the Files panel says why a blocked download is blocked", async ({ page }) => {
    const id = seededRunId();
    await stubPolicy(page, id, "blocked");
    await stubFiles(page, id, "blocked");
    await page.goto(`/runs/${id}`);
    const panel = page.getByTestId("desktop-files");
    await panel.scrollIntoViewIfNeeded();
    await panel.getByRole("tab", { name: C.fromTab }).click();
    await expect(panel.getByText(C.downloadBlocked, { exact: true })).toBeVisible();
    await expect(panel.getByText(DATA_OUT_RUN.FILES_BLOCKED, { exact: true })).toBeVisible();
  });

  test("a run with no desktop and the rule review shows only the reviewed way out", async ({ page }) => {
    const id = seededRunId();
    await stubNoApps(page, id);
    await stubPolicy(page, id, "review");
    await stubFiles(page, id, "review");
    await page.goto(`/runs/${id}`);
    const panel = page.getByTestId("desktop-files");
    await panel.scrollIntoViewIfNeeded();
    await expect(panel.getByText(DATA_OUT_RUN.FILES_NO_DESKTOP, { exact: true })).toBeVisible();
    await expect(panel.getByRole("tab")).toHaveCount(0);
    await expect(panel.getByText(C.pasteIn, { exact: true })).toHaveCount(0);
    await expect(panel.getByRole("button", { name: C.requestDownload })).toBeVisible();
  });
});
