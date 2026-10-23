/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A scheduled run on the Runs list and the run page (FEAT-2 boards F2SCHED-2 and F2SCHED-3): the Scheduled word,
// starts {when} and Cancel run; the due run waiting for its owner's sign-in; and the run that waited it out.
//
// Like runs-landing.spec.ts, the list is replaced through page.route, and like run-queued.spec.ts the run page's
// read is patched: the seeded backend holds no pool to queue a run behind. The times are fixed in UTC so the
// sentences are the boards' own.
import type { Page } from "@playwright/test";
import { test, expect, gotoConsole, navTo } from "./fixtures";
import { SCHEDULED_RUN } from "../src/app/components/wardyn/copy/scheduled-runs";
import { CONNECTIONS } from "../src/app/components/wardyn/copy/door";

const RUNS_LIST_RE = /\/api\/v1\/runs(\?.*)?$/;
const START = "2026-10-21T02:00:00Z";
const DEADLINE = "2026-10-27T14:00:00Z";

const queue = (reason: string) => ({
  pool_id: "pool-e2e",
  queued_at: "2026-10-20T14:00:00Z",
  deadline_at: DEADLINE,
  position: 0,
  reason,
  start_at: START,
  time_zone: "UTC",
});

function baseRun(id: string, title: string, over: Record<string, unknown> = {}): Record<string, unknown> {
  const now = new Date().toISOString();
  return {
    id, task: title, title, created_at: now, updated_at: now, created_by: "admin-token", agent: "claude-code", repo: "acme/web-app",
    confinement_class: "CC2", state: "QUEUED", spiffe_id: "", runner_target: "docker", ...over,
  };
}

async function mockRunsList(page: Page, runs: unknown[]): Promise<void> {
  await page.route(RUNS_LIST_RE, (route) => {
    // The shell's cold-load probe (?limit=1) is not the page's own read.
    if (!new URL(route.request().url()).searchParams.has("view")) return route.fallback();
    return route.fulfill({
      contentType: "application/json",
      headers: { "X-Wardyn-Hidden-Older": "0", "X-Wardyn-Hidden-Killed": "0" },
      body: JSON.stringify(runs),
    });
  });
}

test.describe("Runs list: scheduled runs", () => {
  test("a scheduled run reads Scheduled with starts {when}, and Cancel run asks before it kills", async ({ page }) => {
    await mockRunsList(page, [baseRun("s1", "Nightly dependency bump", { queue: queue("scheduled") })]);
    const kills: string[] = [];
    await page.route("**/api/v1/runs/s1/kill", (route) => {
      kills.push(route.request().method());
      return route.fulfill({ status: 200, json: {} });
    });
    await gotoConsole(page);
    const row = page.getByTestId("run-row").filter({ hasText: "Nightly dependency bump" });
    await expect(row.getByText(SCHEDULED_RUN.WORD, { exact: true })).toBeVisible();
    await expect(row.getByText(SCHEDULED_RUN.ROW_STARTS("Wed 21 Oct, 02:00 (UTC)"), { exact: false })).toBeVisible();
    await expect(row.getByText(/started/)).toHaveCount(0);
    await expect(row.getByText("Queued", { exact: true })).toHaveCount(0);
    await row.getByRole("button", { name: SCHEDULED_RUN.CANCEL_RUN }).click();
    expect(kills).toEqual([]);
    await page.getByRole("button", { name: "Kill run" }).click();
    await expect.poll(() => kills).toEqual(["POST"]);
  });

  test("a due run reads Waiting for sign-in with was due {when} and a Sign in button", async ({ page }) => {
    await mockRunsList(page, [baseRun("s2", "Nightly dependency bump", { queue: queue("owner_sign_in") })]);
    await gotoConsole(page);
    const row = page.getByTestId("run-row");
    await expect(row.getByText("Waiting for sign-in", { exact: true })).toBeVisible();
    await expect(row.getByText(SCHEDULED_RUN.ROW_DUE("Wed 21 Oct, 02:00 (UTC)"), { exact: false })).toBeVisible();
    await expect(row.getByRole("button", { name: "Sign in", exact: true })).toBeVisible();
  });

  test("a run that waited out its sign-in is Failed, with the board's sentence in place of its times", async ({ page }) => {
    await mockRunsList(page, [
      baseRun("s3", "Nightly dependency bump", { state: "FAILED", status_reason: "queue_sign_in_timeout", ended_at: new Date().toISOString() }),
    ]);
    await gotoConsole(page);
    const row = page.getByTestId("run-row").filter({ hasText: "Nightly dependency bump" });
    await expect(row.getByText("Failed", { exact: true })).toBeVisible();
    await expect(row.getByText(SCHEDULED_RUN.SIGN_IN_TIMED_OUT, { exact: false })).toBeVisible();
    await expect(row.getByText(/ran /)).toHaveCount(0);
  });
});

test.describe("Run page: scheduled runs", () => {
  async function patchFirstRun(page: Page, patch: Record<string, unknown>) {
    await page.route("**/api/v1/runs/*", async (route) => {
      if (route.request().method() !== "GET") return route.fallback();
      const response = await route.fetch();
      const json = await response.json();
      if (json.task === "e2e fixture 0") Object.assign(json, patch);
      await route.fulfill({ response, json });
    });
    await gotoConsole(page);
    await navTo(page, "Runs");
    await page.getByText("e2e fixture 0").click();
    await expect(page).toHaveURL(/\/runs\/[0-9a-f-]{8,}/);
  }

  test("a scheduled run is badged Scheduled and says when it starts, with Cancel run", async ({ page }) => {
    await patchFirstRun(page, { state: "QUEUED", interactive: true, queue: queue("scheduled") });
    const hero = page.getByTestId("run-scheduled-wait");
    await expect(hero.getByRole("heading", { name: SCHEDULED_RUN.WORD })).toBeVisible();
    await expect(hero.getByText(/^Starts Wed 21 Oct, 02:00 \(UTC\)/)).toBeVisible();
    await expect(hero.getByText(SCHEDULED_RUN.PAGE_NOTE)).toBeVisible();
    await expect(hero.getByRole("button", { name: SCHEDULED_RUN.CANCEL_RUN })).toBeVisible();
    await expect(page.getByText("Queued", { exact: true })).toHaveCount(0);
  });

  test("a due run says whose sign-in it waits for, and the day it fails without starting", async ({ page }) => {
    await patchFirstRun(page, { state: "QUEUED", interactive: true, queue: queue("owner_sign_in"), model_provider_name: "Bedrock" });
    const hero = page.getByTestId("run-sign-in-wait");
    await expect(hero.getByRole("heading", { name: SCHEDULED_RUN.SIGN_IN_TITLE })).toBeVisible();
    await expect(hero.getByText(CONNECTIONS.C6_LINE("Bedrock"))).toBeVisible();
    await expect(hero.getByText(SCHEDULED_RUN.SIGN_IN_BODY("Wed 21 Oct, 02:00 (UTC)", "Tue 27 Oct, 14:00 (UTC)"))).toBeVisible();
    await expect(hero.getByRole("button", { name: SCHEDULED_RUN.CANCEL_RUN })).toBeVisible();
  });
});
