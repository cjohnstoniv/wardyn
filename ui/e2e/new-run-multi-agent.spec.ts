/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New Run, several agents in one Autonomous run (F2MULTI-1): with the server's switch on, the Harnesses step adds
// a second agent with its own task, refuses a combination no image carries, and the run is created with each
// agent's task on the wire. The hermetic backend holds no pools, so pools, preflight and the create are stubbed
// as in new-run-guided.spec.ts, and the roster and the switch are spliced onto the real /setup/status.
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
import { SCENARIO_HARNESSES } from "../src/app/lib/new-run-fixtures/scenarios/harnesses";
import { SCENARIO_POOLS } from "../src/app/lib/new-run-fixtures/scenarios/pools";
import { SCENARIO_PREFLIGHT } from "../src/app/lib/new-run-fixtures/scenarios/preflight";

const RUN_ID = "a2222222-2222-4222-8222-222222222222";
const [CLAUDE, CODEX] = SCENARIO_HARNESSES.full;
const GEMINI = { ...CODEX, id: "gemini-cli", display: "Gemini CLI" };

async function stubFlow(page: Page): Promise<{ create: string[] }> {
  const bodies = { create: [] as string[] };
  page.on("request", (req) => {
    if (req.method() === "POST" && new URL(req.url()).pathname === "/api/v1/runs") bodies.create.push(req.postData() ?? "");
  });
  let status: Record<string, unknown> | null = null;
  await page.route("**/api/v1/setup/status*", async (route) => {
    status ??= {
      ...((await (await route.fetch()).json()) as Record<string, unknown>),
      harnesses: [CLAUDE, CODEX, GEMINI],
      harness_sets: [[CLAUDE.id, CODEX.id]],
      features: { multi_agent_tasks: true },
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
  await page.route("**/api/v1/runs", (route) =>
    route.request().method() === "POST" ? route.fulfill({ status: 201, json: { id: RUN_ID, state: "PENDING" } }) : route.fallback(),
  );
  await page.route(`**/api/v1/runs/${RUN_ID}`, (route) => route.fulfill({ json: { id: RUN_ID, state: "PENDING" } }));
  return bodies;
}

const press = (page: Page, name: string) => page.getByRole("button", { name, exact: true }).click();
const next = (page: Page, step: string) => press(page, NR_SHARED.CONTINUE_TO(step));

async function toAutonomousHarnesses(page: Page) {
  await gotoConsole(page);
  await navToRoute(page, "/runs/new");
  await page.getByRole("button", { name: NR_START.GUIDED_FOOT }).click();
  await press(page, NR_SHAPE.AGENT_TITLE);
  await page.getByRole("radio", { name: NR_RUN.MODE_AUTONOMOUS }).click();
  await next(page, NR_SHARED.STEP_HARNESSES);
  await expect(page.getByRole("heading", { name: NR_HARNESSES.TITLE })).toBeVisible();
  await expect(page.getByText(NR_HARNESSES.LEDE_AUTONOMOUS_MULTI)).toBeVisible();
}

test.describe("New Run, several agents in one run", () => {
  test("a second agent with its own task is created with each agent's task on the wire", async ({ page }) => {
    const bodies = await stubFlow(page);
    await toAutonomousHarnesses(page);
    await expect(page.getByText(NR_SHARED.COMING_SOON)).toHaveCount(0);
    await page.locator("#nr-task").fill("Fix the flaky login test in web-app.");
    await press(page, NR_HARNESSES.ADD_ANOTHER);
    await expect(page.getByRole("region", { name: NR_HARNESSES.EYEBROW(2) })).toBeVisible();
    await expect(page.getByText(NR_HARNESSES.MULTI_NOTE)).toBeVisible();

    // The second agent has no task yet: Continue stops on it, with the per-card error.
    await next(page, NR_SHARED.STEP_FILES);
    await expect(page.locator("#nr-task-1")).toBeFocused();
    await expect(page.getByText(NR_HARNESSES.AGENT_TASK_ERROR)).toBeVisible();
    await page.locator("#nr-task-1").fill("Write tests that cover the login retry path.");

    await next(page, NR_SHARED.STEP_FILES);
    await page.getByRole("checkbox", { name: NR_FILES.NO_EXTRA }).check();
    await fillMainDriveSize(page);
    await next(page, NR_SHARED.STEP_TOOLS);
    await press(page, NR_TOOLS.REVIEW_RUN);
    await expect(page.getByRole("button", { name: NR_REVIEW.LAUNCH_RUN })).toBeEnabled();
    await page.getByRole("button", { name: NR_REVIEW.LAUNCH_RUN }).click();
    const dialog = page.getByRole("dialog", { name: NR_LAUNCH.TITLE });
    await dialog.getByLabel(NR_LAUNCH.TITLE_LABEL).fill("two agents");
    await dialog.getByRole("button", { name: NR_LAUNCH.LAUNCH, exact: true }).click();
    await expect(page).toHaveURL(new RegExp(`launched=${RUN_ID}`));

    expect(bodies.create).toHaveLength(1);
    const body = JSON.parse(bodies.create[0]);
    expect(body.workload).toEqual({ kind: "agent_task", agent: CLAUDE.id, task: "Fix the flaky login test in web-app." });
    expect(body.tools).toEqual([
      { id: CLAUDE.id, kind: "harness" },
      { id: CODEX.id, kind: "harness", task: "Write tests that cover the login retry path." },
    ]);
  });

  test("a harness no image carries with the others is refused on both cards and holds Continue", async ({ page }) => {
    await stubFlow(page);
    await toAutonomousHarnesses(page);
    await press(page, NR_HARNESSES.ADD_ANOTHER);
    await page.locator("#nr-harness-1").selectOption({ label: "Gemini CLI" });
    await expect(page.getByText(NR_HARNESSES.NO_COMBINATION("Claude Code", "Gemini CLI"))).toBeVisible();
    await expect(page.getByText(NR_HARNESSES.NO_COMBINATION("Gemini CLI", "Claude Code"))).toBeVisible();
    await page.locator("#nr-task").fill("a");
    await page.locator("#nr-task-1").fill("b");
    await next(page, NR_SHARED.STEP_FILES);
    await expect(page.getByRole("heading", { name: NR_HARNESSES.TITLE })).toBeVisible();
    await expect(page.locator("#nr-harness-0")).toBeFocused();
  });
});
