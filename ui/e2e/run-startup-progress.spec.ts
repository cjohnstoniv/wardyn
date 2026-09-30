/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, gotoConsole, navTo } from "./fixtures";
import {
  CHIP_DOWNLOADING,
  PENDING_NO_DETAIL,
  RUN_STARTUP,
  STARTING_UNSCHEDULABLE,
  STUCK_CRASH_LOOP,
  STUCK_IMAGE_PULL,
} from "../src/app/components/screens/run-status-detail";
import { SIGNIN_PROGRESS } from "../src/app/components/screens/settings/login-pane-copy";
import { RUN_COCKPIT } from "../src/app/components/wardyn/copy/run-cockpit";
import { RUN_MODE } from "../src/app/components/wardyn/copy";
import type { Page } from "@playwright/test";

// #1419: the run page's startup view. The seeded backend has no real substrate
// behind its PENDING (fixture 0) and STARTING (fixture 1) runs, so each case
// splices what the substrate would have reported onto the GET the console
// actually makes (route.fetch + patch + refulfill, the shape runs-header.spec.ts
// uses). Every seeded fixture is autonomous (scripts/e2e-backend.sh creates
// them without `interactive`), so a "Terminal" / "Opening the terminal" case
// splices interactive=true in the same patch, as runs-detail.spec.ts does; the
// "Output" / "Starting the task" case is the unspliced default.
test.describe.configure({ mode: "serial" });

const S = 1000;
const MIN = 60 * S;

type Splice = Record<string, unknown> & { ageUpdated?: number; ageCreated?: number };

// Splice `patch()` onto fixture `task`'s run read. `patch` is read on every
// poll, so a case can change the run mid-visit. ageUpdated / ageCreated are
// "this many ms ago", recomputed per read so the page's clock and the run's
// timestamps stay in the same relation however long the case takes.
async function spliceRun(page: Page, task: string, patch: () => Splice): Promise<void> {
  await page.route("**/api/v1/runs/*", async (route) => {
    if (route.request().method() !== "GET") return route.fallback();
    const response = await route.fetch();
    const json = await response.json();
    if (json.task === task) {
      const { ageUpdated, ageCreated, ...fields } = patch();
      Object.assign(json, fields);
      if (ageUpdated !== undefined) json.updated_at = new Date(Date.now() - ageUpdated).toISOString();
      if (ageCreated !== undefined) json.created_at = new Date(Date.now() - ageCreated).toISOString();
    }
    await route.fulfill({ response, json });
  });
}

async function openRun(page: Page, task: string): Promise<void> {
  await gotoConsole(page);
  await navTo(page, "Runs");
  await expect(page.getByRole("heading", { name: "Runs", level: 1 })).toBeVisible();
  await page.getByText(task).click();
  await expect(page).toHaveURL(/\/runs\/[0-9a-f-]{8,}/);
}

const pane = (page: Page) => page.getByTestId("run-terminal-pane");
const list = (page: Page) => pane(page).getByTestId("run-startup-progress");
const rows = (page: Page) => list(page).getByRole("listitem");
const hint = (page: Page) => pane(page).getByRole("status");

async function expectRows(page: Page, expected: Array<[string, string]>): Promise<void> {
  await expect(rows(page)).toHaveCount(expected.length);
  for (const [i, [label, state]] of expected.entries()) {
    await expect(rows(page).nth(i)).toHaveText(label);
    await expect(rows(page).nth(i)).toHaveAttribute("data-state", state);
  }
}

const PULLING = { status_detail: "image: Pulling: wardyn/agent:1", status_reason: "Pulling" };
const BUILDING = { status_detail: "image: Building", status_reason: "Building" };

test.describe("Run page — startup progress in the terminal hero (#1419)", () => {
  test("PENDING: the list is there, and the hero does not repeat the Queued line", async ({ page }) => {
    await spliceRun(page, "e2e fixture 0", () => ({ interactive: true, status_detail: "", status_reason: "" }));
    await openRun(page, "e2e fixture 0");

    await expectRows(page, [
      [RUN_STARTUP.STEP_START, "active"],
      [CHIP_DOWNLOADING, "pending"],
      [RUN_STARTUP.STEP_TERMINAL, "pending"],
    ]);
    await expect(rows(page).first()).toHaveAttribute("aria-current", "step");
    await expect(pane(page).getByText("Terminal", { exact: true })).toBeVisible();
    // Strict mode: a second copy of the sentence would fail this locator.
    await expect(page.getByText(PENDING_NO_DETAIL, { exact: true })).toBeVisible();
    await expect(pane(page).getByText(PENDING_NO_DETAIL)).toHaveCount(0);
    await expect(hint(page)).toHaveCount(0);
    // The retired notice is gone.
    await expect(page.getByText("This run hasn't started yet", { exact: false })).toHaveCount(0);
  });

  test("Building: row, hint and header chip; the same visit then STARTING shows Build done", async ({ page }) => {
    let stage: Splice = { interactive: true, ...BUILDING, ageCreated: 5 * S };
    await spliceRun(page, "e2e fixture 0", () => stage);
    await openRun(page, "e2e fixture 0");

    await expectRows(page, [
      [RUN_STARTUP.STEP_BUILD, "active"],
      [RUN_STARTUP.STEP_START, "pending"],
      [CHIP_DOWNLOADING, "pending"],
      [RUN_STARTUP.STEP_TERMINAL, "pending"],
    ]);
    await expect(hint(page)).toHaveText(RUN_STARTUP.BUILD_HINT);
    await expect(page.getByTestId("run-summary-header").getByText(RUN_STARTUP.STEP_BUILD)).toBeVisible();

    // The build finished and the sandbox is coming up: the server blanks the
    // line for STARTING, and the page remembers it saw the build.
    stage = { interactive: true, state: "STARTING", status_detail: "", status_reason: "", ageUpdated: 3 * S, ageCreated: 10 * MIN };
    await expect(rows(page).first()).toHaveAttribute("data-state", "done", { timeout: 10_000 });
    await expectRows(page, [
      [RUN_STARTUP.STEP_BUILD, "done"],
      [RUN_STARTUP.STEP_START, "active"],
      [CHIP_DOWNLOADING, "pending"],
      [RUN_STARTUP.STEP_TERMINAL, "pending"],
    ]);
    // A 10-minute build just ended: the minute counts from when Starting began.
    await expect(hint(page)).toHaveCount(0);
  });

  test("Pulling: done / active and the hint; a reload shows the same state (without a Build row)", async ({ page }) => {
    await spliceRun(page, "e2e fixture 1", () => ({ interactive: true, ...PULLING, ageUpdated: 4 * S }));
    await openRun(page, "e2e fixture 1");

    const expected: Array<[string, string]> = [
      [RUN_STARTUP.STEP_START, "done"],
      [CHIP_DOWNLOADING, "active"],
      [RUN_STARTUP.STEP_TERMINAL, "pending"],
    ];
    await expectRows(page, expected);
    await expect(hint(page)).toHaveText(SIGNIN_PROGRESS.DOWNLOAD_HINT);

    await page.reload();
    await expectRows(page, expected);
    await expect(hint(page)).toHaveText(SIGNIN_PROGRESS.DOWNLOAD_HINT);
  });

  test("STARTING for 2 minutes with nothing reported: the SLOW line", async ({ page }) => {
    await spliceRun(page, "e2e fixture 1", () => ({
      interactive: true,
      status_detail: "",
      status_reason: "",
      ageUpdated: 2 * MIN,
    }));
    await openRun(page, "e2e fixture 1");
    await expect(hint(page)).toHaveText(RUN_STARTUP.SLOW);
    await expect(rows(page).first()).toHaveAttribute("data-state", "active");
  });

  test("STARTING + Unschedulable for 2 minutes: the machine sentence", async ({ page }) => {
    await spliceRun(page, "e2e fixture 1", () => ({
      interactive: true,
      status_detail: "pod: Unschedulable: 0/1 nodes are available",
      status_reason: "Unschedulable",
      ageUpdated: 2 * MIN,
    }));
    await openRun(page, "e2e fixture 1");
    await expect(hint(page)).toHaveText(STARTING_UNSCHEDULABLE);
  });

  test("created 10 minutes ago but STARTING only just began: no hint", async ({ page }) => {
    await spliceRun(page, "e2e fixture 1", () => ({
      interactive: true,
      status_detail: "",
      status_reason: "",
      ageCreated: 10 * MIN,
      ageUpdated: 5 * S,
    }));
    await openRun(page, "e2e fixture 1");
    await expect(rows(page).first()).toHaveAttribute("data-state", "active");
    await expect(hint(page)).toHaveCount(0);
  });

  test("overdue in STARTING: the Download row loses its spinner and the hint says what to do", async ({ page }) => {
    await spliceRun(page, "e2e fixture 1", () => ({ interactive: true, ...PULLING, ageUpdated: 5 * MIN }));
    await openRun(page, "e2e fixture 1");
    await expect(hint(page)).toHaveText(RUN_STARTUP.OVERDUE);
    await expectRows(page, [
      [RUN_STARTUP.STEP_START, "done"],
      [CHIP_DOWNLOADING, "pending"],
      [RUN_STARTUP.STEP_TERMINAL, "pending"],
    ]);
    await expect(list(page).locator('[data-state="active"]')).toHaveCount(0);
  });

  test("overdue in PENDING: 31 minutes queued", async ({ page }) => {
    await spliceRun(page, "e2e fixture 0", () => ({ interactive: true, ...BUILDING, ageCreated: 31 * MIN }));
    await openRun(page, "e2e fixture 0");
    await expect(hint(page)).toHaveText(RUN_STARTUP.OVERDUE);
    await expect(rows(page).first()).toHaveText(RUN_STARTUP.STEP_BUILD);
    await expect(list(page).locator('[data-state="active"]')).toHaveCount(0);
  });

  test("ImagePullBackOff: the failed row and the pull alert, the list stops", async ({ page }) => {
    await spliceRun(page, "e2e fixture 1", () => ({
      interactive: true,
      status_detail: "agent: ImagePullBackOff: rpc error: pull access denied",
      status_reason: "ImagePullBackOff",
      ageUpdated: 5 * S,
    }));
    await openRun(page, "e2e fixture 1");
    await expectRows(page, [
      [RUN_STARTUP.STEP_START, "done"],
      [RUN_STARTUP.STEP_DOWNLOAD_FAILED, "failed"],
    ]);
    await expect(pane(page).getByRole("alert")).toContainText(`${STUCK_IMAGE_PULL} rpc error: pull access denied`);
    await expect(hint(page)).toHaveCount(0);
  });

  test("CrashLoopBackOff: Starting the sandbox failed, and its alert", async ({ page }) => {
    await spliceRun(page, "e2e fixture 1", () => ({
      interactive: true,
      status_detail: "agent: CrashLoopBackOff: back-off 20s restarting failed container",
      status_reason: "CrashLoopBackOff",
      ageUpdated: 5 * S,
    }));
    await openRun(page, "e2e fixture 1");
    await expectRows(page, [[RUN_STARTUP.STEP_START_FAILED, "failed"]]);
    await expect(pane(page).getByRole("alert")).toContainText(STUCK_CRASH_LOOP);
  });

  test("the default (autonomous) fixture: Output, its chip, and Starting the task", async ({ page }) => {
    await spliceRun(page, "e2e fixture 1", () => ({ ...PULLING, ageUpdated: 4 * S }));
    await openRun(page, "e2e fixture 1");
    await expect(pane(page).getByText("Output", { exact: true })).toBeVisible();
    await expect(pane(page).getByText(RUN_COCKPIT.autonomous)).toBeVisible();
    await expectRows(page, [
      [RUN_STARTUP.STEP_START, "done"],
      [CHIP_DOWNLOADING, "active"],
      [RUN_STARTUP.STEP_TASK, "pending"],
    ]);
  });

  test("RUNNING: the list is gone", async ({ page }) => {
    await openRun(page, "e2e fixture 3");
    // Wait for the RUNNING pane itself (the autonomous notice), then assert the
    // list is absent, so the absence is not just "the page had not rendered".
    await expect(pane(page).getByText(RUN_MODE.autonomous.blurb)).toBeVisible();
    await expect(page.getByTestId("run-startup-progress")).toHaveCount(0);
  });
});
