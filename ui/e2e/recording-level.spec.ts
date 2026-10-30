/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #2968: recording as a per-policy level. The policy editor's Recording section (M7 Knobs-B, States
// 1 and 3) and the run's Recording tab (M7 RunStrip, States 8; the desktop replay is in
// desktop-replay.spec.ts). The seeded `none` runner never records, so the level is spliced on the way
// past; what each level does to a real run is pinned in Go (internal/api/run_recording_level_test.go).
import { randomUUID } from "node:crypto";
import type { Page, Route } from "@playwright/test";
import { test, expect, consoleAPI, gotoConsole, navTo, navToRoute } from "./fixtures";
import { RECORDING_LEVEL, RECORDING_REPLAY, RECORDING_STRIP } from "../src/app/components/wardyn/copy/recording-level";
import type { RunPolicySpec } from "../src/app/lib/types";

async function seedRun(page: Page): Promise<string> {
  const created = await consoleAPI(page, "POST", "/api/v1/runs", { agent: "claude-code", task: "recording-level e2e " + randomUUID().slice(0, 8) });
  expect(created.status, created.text).toBe(201);
  return JSON.parse(created.text).id as string;
}

// Splices fields onto the run's own GET /runs/{id} answer, leaving every other field real.
async function spliceRun(page: Page, id: string, patch: Record<string, unknown>): Promise<void> {
  await page.route(new RegExp(`/api/v1/runs/${id}$`), async (route: Route) => {
    if (route.request().method() !== "GET") return route.continue();
    const res = await route.fetch();
    const body = (await res.json()) as Record<string, unknown>;
    await route.fulfill({ response: res, json: { ...body, ...patch } });
  });
}

async function openRecordingTab(page: Page, id: string): Promise<void> {
  await navToRoute(page, `/runs/${id}`);
  await page.getByRole("tab", { name: "Recording" }).click();
  await expect(page.getByRole("tab", { name: "Recording" })).toHaveAttribute("aria-selected", "true");
}

test.describe("Recording levels", () => {
  test("the policy editor writes the level, keystrokes are a fixed line, a locked minimum strikes out Off", async ({ page }) => {
    await page.route("**/api/v1/site-config", async (route: Route) => {
      if (route.request().method() !== "GET") return route.continue();
      const res = await route.fetch();
      const body = (await res.json()) as Record<string, unknown>;
      await route.fulfill({ response: res, json: { ...body, recording: { level: "terminal", locked: true } } });
    });
    await gotoConsole(page, "admin");
    await navTo(page, "Policies");
    await page.getByRole("button", { name: "New policy" }).first().click();
    const dialog = page.getByRole("dialog").filter({ hasText: "New policy" });
    const section = dialog.getByTestId("policy-recording");
    const group = section.getByRole("radiogroup", { name: RECORDING_LEVEL.ROW });
    await expect(group.getByRole("radio", { name: "Off" })).toBeDisabled();
    await expect(section.getByTestId("policy-recording-floor")).toHaveText(
      "Your organisation requires at least Terminal. Off isn't available here.",
    );
    await expect(section.getByTestId("policy-recording-keystrokes")).toContainText(RECORDING_LEVEL.KEYSTROKES_VALUE);
    await expect(section.getByRole("radio", { name: /keystroke/i })).toHaveCount(0);

    await group.getByRole("radio", { name: "Screen and actions" }).click();
    await expect(group.getByRole("radio", { name: "Screen and actions" })).toHaveAttribute("aria-checked", "true");
    await expect(section).toContainText(RECORDING_LEVEL.LEVELS[2].description);

    // What reaches the server, not the editor's opinion of it.
    await dialog.getByLabel("Name", { exact: true }).fill(`recording-${Date.now().toString(36)}`);
    const created = page.waitForRequest((r) => r.url().includes("/api/v1/policies") && r.method() === "POST");
    await dialog.getByRole("button", { name: "Create policy" }).click();
    const body = (await created).postDataJSON() as { spec: RunPolicySpec };
    expect(body.spec.recording).toBe("screen_actions");
  });

  test("a run resolved to off says so in the run strip and the empty state", async ({ page }) => {
    await gotoConsole(page, "admin");
    const id = await seedRun(page);
    await spliceRun(page, id, { recording_level: "off", not_recorded: "policy_off" });
    await openRecordingTab(page, id);
    const strip = page.getByTestId("recording-level-strip");
    await expect(strip).toContainText(RECORDING_STRIP.OFF);
    await expect(strip).toContainText(RECORDING_STRIP.OFF_NOTE);
    await expect(page.getByText(RECORDING_REPLAY.OFF_BY_POLICY)).toBeVisible();
  });
});
