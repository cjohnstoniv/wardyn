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
import { test, expect, consoleAPI, gotoConsole, navToRoute } from "./fixtures";

const BLOCKED = "This run's data_out rule keeps its content in: only a super admin can replay its recording.";
const UNREADABLE = "This run's data_out rule could not be read. Try again.";

async function seedRun(page: Page): Promise<string> {
  const created = await consoleAPI(page, "POST", "/api/v1/runs", {
    agent: "claude-code",
    task: "data-out run e2e " + randomUUID().slice(0, 8),
    min_confinement_class: "CC3",
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
});
