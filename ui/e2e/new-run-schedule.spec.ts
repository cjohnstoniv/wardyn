/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New Run, Schedule for later (FEAT-2 board F2SCHED-1): the start-later control on the Run step, the Review row,
// the Starts line on Launch, the start in the create body, and the refusals a start can draw. Shown only while
// the setup status carries features.scheduled_runs.
//
// The seeded backend runs `-runner none` and holds no pools, and has the switch off: the pool list, the preflight
// answer and the create are stubbed, and the switch is spliced onto the real /setup/status.
import type { Page } from "@playwright/test";
import { test, expect, fillMainDriveSize, gotoConsole, navToRoute } from "./fixtures";
import { NR_FILES } from "../src/app/components/wardyn/copy/new-run/files";
import { NR_HARNESSES } from "../src/app/components/wardyn/copy/new-run/harnesses";
import { NR_LAUNCH } from "../src/app/components/wardyn/copy/new-run/launch";
import { NR_REVIEW } from "../src/app/components/wardyn/copy/new-run/review";
import { NR_RUN } from "../src/app/components/wardyn/copy/new-run/run";
import { NR_SHAPE } from "../src/app/components/wardyn/copy/new-run/shape";
import { NR_SHARED } from "../src/app/components/wardyn/copy/new-run/shared";
import { NR_START } from "../src/app/components/wardyn/copy/new-run/start";
import { NR_TOOLS } from "../src/app/components/wardyn/copy/new-run/tools";
import { SCHEDULED_RUN } from "../src/app/components/wardyn/copy/scheduled-runs";
import { SCENARIO_HARNESSES, SCENARIO_HARNESS_SETS } from "../src/app/lib/new-run-fixtures/scenarios/harnesses";
import { SCENARIO_POOLS } from "../src/app/lib/new-run-fixtures/scenarios/pools";
import { SCENARIO_PREFLIGHT } from "../src/app/lib/new-run-fixtures/scenarios/preflight";
import { scheduleDaysAhead } from "../src/app/lib/schedule";
import { SCHEDULE_REFUSAL } from "../src/app/lib/schedule-refusals";

const RUN_ID = "a1111111-1111-4111-8111-111111111111";
const DAY = 86_400_000;
const dayAhead = (n: number) => new Date(Date.now() + n * DAY).toISOString().slice(0, 10);

interface Stub {
  create: string[];
  preflight: string[];
  /** What the create answers: the queued scheduled run, or a refusal. */
  refuse?: { status: number; json: object };
}

async function stub(page: Page, scheduledRuns: boolean | undefined): Promise<Stub> {
  const seen: Stub = { create: [], preflight: [] };
  page.on("request", (req) => {
    if (req.method() !== "POST") return;
    const path = new URL(req.url()).pathname;
    if (path === "/api/v1/runs/preflight") seen.preflight.push(req.postData() ?? "");
    else if (path === "/api/v1/runs") seen.create.push(req.postData() ?? "");
  });
  let status: Record<string, unknown> | null = null;
  await page.route("**/api/v1/setup/status*", async (route) => {
    status ??= {
      ...((await (await route.fetch()).json()) as Record<string, unknown>),
      harnesses: SCENARIO_HARNESSES.full,
      harness_sets: SCENARIO_HARNESS_SETS.full,
      ...(scheduledRuns !== undefined && { features: { scheduled_runs: scheduledRuns } }),
    };
    await route.fulfill({ json: status });
  });
  const pools = SCENARIO_POOLS.member.pools.map((p) => ({
    ...p,
    run_types: ["background", "interactive"],
    run_types_source: "pool",
    barrier_options: [
      { class: "CC1", available: true },
      { class: "CC2", available: true, default: true, default_source: "server" },
      { class: "CC3", available: true },
    ],
  }));
  await page.route("**/api/v1/runner-pools*", (route) =>
    route.fulfill({ json: { pools, default: { pool_id: pools[0].id, selection: "organisation_default" } } }),
  );
  await page.route("**/api/v1/runs/preflight", (route) => route.fulfill({ json: SCENARIO_PREFLIGHT.member }));
  await page.route("**/api/v1/runs", (route) => {
    if (route.request().method() !== "POST") return route.fallback();
    return seen.refuse ? route.fulfill(seen.refuse) : route.fulfill({ status: 201, json: { id: RUN_ID, state: "QUEUED" } });
  });
  await page.route(`**/api/v1/runs/${RUN_ID}`, (route) => {
    const start = JSON.parse(seen.create.at(-1) ?? "{}").start_at as string | undefined;
    return route.fulfill({
      json: {
        id: RUN_ID,
        state: "QUEUED",
        title: "nightly",
        queue: {
          pool_id: pools[0].id,
          queued_at: new Date().toISOString(),
          deadline_at: new Date(Date.now() + 7 * DAY).toISOString(),
          position: 0,
          reason: "scheduled",
          start_at: start ? `${start}:00Z` : undefined,
          time_zone: "UTC",
        },
      },
    });
  });
  return seen;
}

const press = (page: Page, name: string) => page.getByRole("button", { name, exact: true }).click();
const next = (page: Page, step: string) => press(page, NR_SHARED.CONTINUE_TO(step));

/** Shape (Agent task) and Run, with Autonomous chosen, on the Run step. */
async function toRunStep(page: Page) {
  await gotoConsole(page);
  await navToRoute(page, "/runs/new");
  await expect(page.getByRole("heading", { name: NR_START.TITLE, level: 1 })).toBeVisible();
  await page.getByRole("button", { name: NR_START.GUIDED_FOOT }).click();
  await press(page, NR_SHAPE.AGENT_TITLE);
  await page.getByRole("radio", { name: NR_RUN.MODE_AUTONOMOUS }).click();
}

async function toReview(page: Page) {
  await next(page, NR_SHARED.STEP_HARNESSES);
  await expect(page.getByRole("heading", { name: NR_HARNESSES.TITLE })).toBeVisible();
  await page.locator("#nr-task").fill("Bump the dependencies");
  await next(page, NR_SHARED.STEP_FILES);
  await page.getByRole("checkbox", { name: NR_FILES.NO_EXTRA }).check();
  await fillMainDriveSize(page);
  await next(page, NR_SHARED.STEP_TOOLS);
  await press(page, NR_TOOLS.REVIEW_RUN);
  await expect(page.getByRole("heading", { name: NR_REVIEW.TITLE })).toBeVisible();
  await expect(page.getByRole("button", { name: NR_REVIEW.LAUNCH_RUN })).toBeEnabled();
}

async function pickStart(page: Page, date: string, time: string) {
  await page.getByRole("radio", { name: NR_RUN.TIMING_LATER }).click();
  await page.getByLabel(NR_RUN.START_ZONE, { exact: true }).selectOption("UTC");
  await page.getByLabel(NR_RUN.START_DATE, { exact: true }).fill(date);
  await page.getByLabel(NR_RUN.START_TIME, { exact: true }).fill(time);
}

test.describe("New Run: Schedule for later", () => {
  test("with the switch off or absent, Schedule for later stays Coming soon and opens nothing", async ({ page }) => {
    for (const off of [false, undefined]) {
      await stub(page, off);
      await toRunStep(page);
      const later = page.getByRole("radio", { name: NR_RUN.TIMING_LATER });
      await expect(later).toBeDisabled();
      await expect(page.getByText(NR_SHARED.COMING_SOON)).toBeVisible();
      await expect(page.getByLabel(NR_RUN.START_DATE, { exact: true })).toHaveCount(0);
      await page.unrouteAll({ behavior: "ignoreErrors" });
    }
  });

  test("on, it opens Date, Time and Time zone, and says the board's sentences for a start that will not do", async ({ page }) => {
    await stub(page, true);
    await toRunStep(page);
    const later = page.getByRole("radio", { name: NR_RUN.TIMING_LATER });
    await expect(later).toBeEnabled();
    await later.click();
    await expect(page.getByText(NR_SHARED.COMING_SOON)).toHaveCount(0);
    await expect(page.getByRole("alert").filter({ hasText: /Choose a/ })).toHaveCount(0);
    await expect(page.getByText(NR_RUN.START_HELP(scheduleDaysAhead()))).toBeVisible();
    await page.getByLabel(NR_RUN.START_TIME, { exact: true }).fill("02:00");
    await expect(page.getByRole("alert").filter({ hasText: NR_RUN.START_MISSING })).toBeVisible();
    await pickStart(page, "2020-01-01", "02:00");
    await expect(page.getByRole("alert").filter({ hasText: NR_RUN.START_PAST })).toBeVisible();
    await pickStart(page, dayAhead(9), "02:00");
    await expect(page.getByRole("alert").filter({ hasText: NR_RUN.START_FAR(scheduleDaysAhead()) })).toBeVisible();
    await pickStart(page, dayAhead(2), "02:00");
    await expect(page.getByRole("alert").filter({ hasText: /Choose a/ })).toHaveCount(0);
    await expect(page.getByRole("status").filter({ hasText: /^Starts [A-Z][a-z]{2} \d{1,2} [A-Z][a-z]{2}, 02:00 \(UTC\)$/ })).toBeVisible();
  });

  test("Review, Launch and the Launched screen carry the start, and the create sends start_at beside the check's body", async ({ page }) => {
    const seen = await stub(page, true);
    await toRunStep(page);
    const date = dayAhead(2);
    await pickStart(page, date, "02:00");
    await toReview(page);
    await expect(page.getByText(new RegExp(`Autonomous · Starts [A-Z][a-z]{2} \\d{1,2} [A-Z][a-z]{2}, 02:00 \\(UTC\\) · ${NR_REVIEW.TIMING_CHECKED.replace(".", "\\.")}`))).toBeVisible();
    await page.getByRole("button", { name: NR_REVIEW.LAUNCH_RUN }).click();
    const dialog = page.getByRole("dialog", { name: NR_LAUNCH.TITLE });
    await dialog.getByLabel(NR_LAUNCH.TITLE_LABEL).fill("nightly");
    await expect(dialog.getByText(NR_LAUNCH.CHECKED_JUST_NOW)).toBeVisible();
    await expect(dialog.getByText(/^Starts [A-Z][a-z]{2} \d{1,2} [A-Z][a-z]{2}, 02:00 \(UTC\)$/)).toBeVisible();
    await dialog.getByRole("button", { name: NR_LAUNCH.LAUNCH, exact: true }).click();
    await expect(page).toHaveURL(new RegExp(`launched=${RUN_ID}`));
    const { title, start_at, start_time_zone, ...create } = JSON.parse(seen.create[0]);
    expect(title).toBe("nightly");
    expect(start_at).toBe(`${date}T02:00`);
    expect(start_time_zone).toBe("UTC");
    expect(create).toEqual(JSON.parse(seen.preflight.at(-1) ?? "null"));
    const hero = page.getByTestId("launched-scheduled");
    await expect(hero).toContainText(SCHEDULED_RUN.PAGE_NOTE);
    await expect(hero).toContainText("Starts");
    await expect(page.getByText(NR_LAUNCH.POSITION)).toHaveCount(0);
  });

  test("the dialog holds a start past 7 days less a background run's 24 hours", async ({ page }) => {
    await stub(page, true);
    await toRunStep(page);
    const iso = new Date(Date.now() + 6.5 * DAY).toISOString();
    await pickStart(page, iso.slice(0, 10), iso.slice(11, 16));
    await toReview(page);
    await page.getByRole("button", { name: NR_REVIEW.LAUNCH_RUN }).click();
    const dialog = page.getByRole("dialog", { name: NR_LAUNCH.TITLE });
    await dialog.getByLabel(NR_LAUNCH.TITLE_LABEL).fill("nightly");
    await expect(dialog.getByText(NR_LAUNCH.CHECKED_JUST_NOW)).toBeVisible();
    await expect(dialog.getByRole("alert").filter({ hasText: NR_RUN.START_FAR(scheduleDaysAhead()) })).toBeVisible();
    await expect(dialog.getByRole("button", { name: NR_LAUNCH.LAUNCH, exact: true })).toBeDisabled();
  });

  test("a start the server refuses shows its sentence in the dialog, never its code", async ({ page }) => {
    const seen = await stub(page, true);
    seen.refuse = {
      status: 422,
      json: { error: SCHEDULE_REFUSAL.TOO_FAR("Tue 27 Oct, 15:00 (UTC)", "Mon 26 Oct, 14:00 (UTC)", 24), reason: "start_at_too_far" },
    };
    await toRunStep(page);
    await pickStart(page, dayAhead(2), "23:00"); // inside the client's limit: this is the server disagreeing
    await toReview(page);
    await page.getByRole("button", { name: NR_REVIEW.LAUNCH_RUN }).click();
    const dialog = page.getByRole("dialog", { name: NR_LAUNCH.TITLE });
    await dialog.getByLabel(NR_LAUNCH.TITLE_LABEL).fill("nightly");
    await expect(dialog.getByText(NR_LAUNCH.CHECKED_JUST_NOW)).toBeVisible();
    await dialog.getByRole("button", { name: NR_LAUNCH.LAUNCH, exact: true }).click();
    await expect(dialog.locator("#name-run-sentence")).toContainText("The latest start is Mon 26 Oct, 14:00 (UTC)");
    await expect(dialog).not.toContainText("start_at_too_far");
  });
});
