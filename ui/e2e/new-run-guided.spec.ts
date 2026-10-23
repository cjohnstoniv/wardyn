/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New Run, Guided setup: each workload from Start to a created run, and the
// Harnesses step. The pin kept from the old one-page spec: the create body,
// minus title and description, is the last preflight body byte for byte.
//
// The seeded backend runs `-runner none` and holds no pools, so the pool list
// and the preflight answer are stubbed from the New Run scenario fixtures; the
// harness roster is spliced onto the real /setup/status. Policy preview and the
// tools-and-access reads go to the real server.
import type { Page } from "@playwright/test";
import { test, expect, fillMainDriveSize, gotoConsole, navToRoute, sidebarLink } from "./fixtures";
import { UNSAVED_GUARD } from "../src/app/components/wardyn/copy/shell";
import { NR_HARNESSES } from "../src/app/components/wardyn/copy/new-run/harnesses";
import { NR_LAUNCH } from "../src/app/components/wardyn/copy/new-run/launch";
import { NR_REVIEW } from "../src/app/components/wardyn/copy/new-run/review";
import { NR_FILES } from "../src/app/components/wardyn/copy/new-run/files";
import { NR_RUN } from "../src/app/components/wardyn/copy/new-run/run";
import { NR_SHAPE } from "../src/app/components/wardyn/copy/new-run/shape";
import { NR_SHARED } from "../src/app/components/wardyn/copy/new-run/shared";
import { NR_START } from "../src/app/components/wardyn/copy/new-run/start";
import { NR_TOOLS } from "../src/app/components/wardyn/copy/new-run/tools";
import { SCENARIO_HARNESSES, SCENARIO_HARNESS_SETS } from "../src/app/lib/new-run-fixtures/scenarios/harnesses";
import { SCENARIO_POOLS } from "../src/app/lib/new-run-fixtures/scenarios/pools";
import { SCENARIO_PREFLIGHT } from "../src/app/lib/new-run-fixtures/scenarios/preflight";
import type { SetupHarnessTool } from "../src/app/lib/types/setup";

const RUN_ID = "a1111111-1111-4111-8111-111111111111";
const [CLAUDE, CODEX] = SCENARIO_HARNESSES.full;

type Bodies = { preflight: string[]; preview: string[]; create: string[] };

/** Stubs what the hermetic backend cannot answer and records the three bodies the flow sends. */
async function stubFlow(page: Page, roster: { harnesses?: SetupHarnessTool[]; sets?: string[][] } = {}): Promise<Bodies> {
  const bodies: Bodies = { preflight: [], preview: [], create: [] };
  page.on("request", (req) => {
    if (req.method() !== "POST") return;
    const path = new URL(req.url()).pathname;
    if (path === "/api/v1/runs/preflight") bodies.preflight.push(req.postData() ?? "");
    else if (path === "/api/v1/runs/policy-preview") bodies.preview.push(req.postData() ?? "");
    else if (path === "/api/v1/runs") bodies.create.push(req.postData() ?? "");
  });
  // Cached and served, never re-fetched per match: the shell's poll and the screen hit this together.
  let status: Record<string, unknown> | null = null;
  await page.route("**/api/v1/setup/status*", async (route) => {
    status ??= {
      ...((await (await route.fetch()).json()) as Record<string, unknown>),
      harnesses: roster.harnesses ?? SCENARIO_HARNESSES.full,
      harness_sets: roster.sets ?? SCENARIO_HARNESS_SETS.full,
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
    route.request().method() === "POST"
      ? route.fulfill({ status: 201, json: { id: RUN_ID, state: "PENDING" } })
      : route.fallback(),
  );
  await page.route(`**/api/v1/runs/${RUN_ID}`, (route) => route.fulfill({ json: { id: RUN_ID, state: "PENDING" } }));
  return bodies;
}

const press = (page: Page, name: string) => page.getByRole("button", { name, exact: true }).click();
const next = (page: Page, step: string) => press(page, NR_SHARED.CONTINUE_TO(step));

async function openGuided(page: Page) {
  await gotoConsole(page);
  await navToRoute(page, "/runs/new");
  await expect(page.getByRole("heading", { name: NR_START.TITLE, level: 1 })).toBeVisible();
  await page.getByRole("button", { name: NR_START.GUIDED_FOOT }).click();
  await expect(page.getByRole("heading", { name: NR_SHAPE.TITLE })).toBeVisible();
}

/** Shape and Run, up to the Harnesses step. The Run step's pool and barrier stay on the server's defaults. */
async function toHarnesses(page: Page, workload: "agent" | "environment" | "application", mode?: "interactive" | "autonomous") {
  const title = { agent: NR_SHAPE.AGENT_TITLE, environment: NR_SHAPE.ENV_TITLE, application: NR_SHAPE.APP_TITLE }[workload];
  // A workload button writes the shape and opens Run.
  await press(page, title);
  if (mode) await page.getByRole("radio", { name: mode === "interactive" ? NR_RUN.MODE_INTERACTIVE : NR_RUN.MODE_AUTONOMOUS }).click();
  if (workload === "application") await page.locator("#nr-command").fill("npm test");
  await next(page, NR_SHARED.STEP_HARNESSES);
  await expect(page.getByRole("heading", { name: NR_HARNESSES.TITLE })).toBeVisible();
}

/** Files and Tools with nothing extra, then Review once the server's check has landed. */
async function toReview(page: Page) {
  await next(page, NR_SHARED.STEP_FILES);
  await page.getByRole("checkbox", { name: NR_FILES.NO_EXTRA }).check();
  await fillMainDriveSize(page);
  await next(page, NR_SHARED.STEP_TOOLS);
  await press(page, NR_TOOLS.REVIEW_RUN);
  await expect(page.getByRole("heading", { name: NR_REVIEW.TITLE })).toBeVisible();
  await expect(page.getByRole("button", { name: NR_REVIEW.LAUNCH_RUN })).toBeEnabled();
}

async function launchThroughNameDialog(page: Page, title: string) {
  await page.getByRole("button", { name: NR_REVIEW.LAUNCH_RUN }).click();
  const dialog = page.getByRole("dialog", { name: NR_LAUNCH.TITLE });
  await dialog.getByLabel(NR_LAUNCH.TITLE_LABEL).fill(title);
  await expect(dialog.getByText(NR_LAUNCH.CHECKED_JUST_NOW)).toBeVisible();
  await dialog.getByRole("button", { name: NR_LAUNCH.LAUNCH, exact: true }).click();
  await expect(page).toHaveURL(new RegExp(`launched=${RUN_ID}`));
}

/** E1: nothing but the Name dialog's fields separates the last check from the create. */
function expectCreateEqualsLastCheck(bodies: Bodies) {
  expect(bodies.create).toHaveLength(1);
  const { title, description, ...create } = JSON.parse(bodies.create[0]);
  expect(title).toBeTruthy();
  void description;
  expect(create).toEqual(JSON.parse(bodies.preflight.at(-1) ?? "null"));
  expect(create).toEqual(JSON.parse(bodies.preview.at(-1) ?? "null"));
  // Untouched, the default policy applies: the run names no policy of its own.
  expect(create).not.toHaveProperty("policy_id");
  expect(create).not.toHaveProperty("policy_overlay");
  return create;
}

test.describe("New Run, Start and Run: the shared pieces", () => {
  test("Start has two real paths and one muted roadmap line, no hatched Coming soon card (#3045)", async ({ page }) => {
    await stubFlow(page);
    await gotoConsole(page);
    await navToRoute(page, "/runs/new");
    await expect(page.getByRole("article")).toHaveCount(0);
    await expect(page.getByRole("button", { name: new RegExp(`^${NR_START.GUIDED_TITLE}`) })).toBeVisible();
    await expect(page.getByRole("region", { name: NR_START.DOC_TITLE })).toBeVisible();
    await expect(page.getByText("AI composer")).toHaveCount(0);
    await expect(page.getByText(NR_SHARED.COMING_SOON)).toHaveCount(0);
    await expect(page.getByText(NR_START.ROADMAP, { exact: true })).toBeVisible();
    await expect(page.getByText(NR_START.ROADMAP)).not.toContainText(/\d/);
  });

  test("a barrier's info tip opens on click and closes on Esc; a caveat is text, not only a hover title (FR-0119, FR-0123)", async ({ page }) => {
    await stubFlow(page);
    await openGuided(page);
    await press(page, NR_SHAPE.AGENT_TITLE);
    const table = page.getByRole("table");
    const caveat = table.locator('[aria-label="Yes, with caveat"]').first();
    const risk = await caveat.getAttribute("title");
    expect(risk).toBeTruthy();
    // The residual risk is in the cell's text, which a screen reader reads, as well as in its title.
    await expect(caveat).toHaveText(risk!);

    const tip = table.getByRole("button", { name: /^About / }).first();
    await tip.click();
    const bubble = page.getByText(NR_RUN.PROTECTS_HEAD);
    await expect(bubble).toBeVisible();
    await expect(page.getByText(NR_RUN.NOT_PROTECTS_HEAD)).toBeVisible();
    await expect(page.getByText(NR_RUN.HOW_HEAD)).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(bubble).toHaveCount(0);
  });
});

test.describe("New Run, Guided setup: each workload end to end", () => {
  test("Agent task, Interactive: a harness starts with the run", async ({ page }) => {
    const bodies = await stubFlow(page);
    await openGuided(page);
    await toHarnesses(page, "agent", "interactive");
    // A new agent task opens with the roster's first harness added and starting the run.
    await expect(page.getByRole("switch", { name: NR_HARNESSES.START_IN_HARNESS })).toBeChecked();
    await page.getByLabel(NR_HARNESSES.STARTING_PROMPT).fill("Find the flaky retry");
    await toReview(page);
    await launchThroughNameDialog(page, "guided interactive");
    expect(expectCreateEqualsLastCheck(bodies)).toMatchObject({
      mode_contract: "0.9",
      shape: "agent_task",
      experience: "interactive",
      startup: { kind: "harness", tool: CLAUDE.id, prompt: "Find the flaky retry" },
    });
  });

  test("Agent task, Autonomous: one harness and a task", async ({ page }) => {
    const bodies = await stubFlow(page);
    await openGuided(page);
    await toHarnesses(page, "agent", "autonomous");
    await page.locator("#nr-task").fill("Fix the flaky retry");
    await toReview(page);
    await launchThroughNameDialog(page, "guided autonomous");
    expect(expectCreateEqualsLastCheck(bodies)).toMatchObject({
      shape: "agent_task",
      experience: "background",
      workload: { kind: "agent_task", agent: CLAUDE.id, task: "Fix the flaky retry" },
    });
  });

  test("Environment sandbox: no harness is needed", async ({ page }) => {
    const bodies = await stubFlow(page);
    await openGuided(page);
    await toHarnesses(page, "environment");
    await expect(page.getByText(NR_HARNESSES.EMPTY_TITLE)).toBeVisible();
    await toReview(page);
    await launchThroughNameDialog(page, "guided environment");
    expect(expectCreateEqualsLastCheck(bodies)).toMatchObject({ shape: "environment", experience: "interactive" });
  });

  test("Application sandbox: a command, harnesses optional", async ({ page }) => {
    const bodies = await stubFlow(page);
    await openGuided(page);
    await toHarnesses(page, "application");
    await expect(page.getByText(NR_HARNESSES.LEDE_OPTIONAL)).toBeVisible();
    await toReview(page);
    await launchThroughNameDialog(page, "guided application");
    expect(expectCreateEqualsLastCheck(bodies)).toMatchObject({
      shape: "application",
      experience: "background",
      workload: { kind: "command", command: "npm test" },
    });
  });
});

test.describe("New Run, Guided setup: the Harnesses step (NR-11)", () => {
  // The last set is the lane's own: Claude Code with Codex CLI is carried by one declared image.
  test("Autonomous holds one harness and one task, with no add control and no Coming soon (#3045)", async ({ page }) => {
    await stubFlow(page);
    await openGuided(page);
    await toHarnesses(page, "agent", "autonomous");
    await expect(page.getByText(NR_HARNESSES.LEDE_AUTONOMOUS)).toBeVisible();
    await expect(page.getByRole("button", { name: NR_HARNESSES.ADD_ANOTHER })).toHaveCount(0);
    await expect(page.getByText(NR_SHARED.COMING_SOON)).toHaveCount(0);
    // #3046: the picker is the card's title, so the old "Harness 1" eyebrow text is gone.
    const card = page.getByRole("region", { name: NR_HARNESSES.EYEBROW(1) });
    await expect(card.locator("#nr-harness-0")).toBeVisible();
    await expect(card.getByText(NR_HARNESSES.EYEBROW(1), { exact: true })).toHaveCount(0);
    await expect(page.getByRole("button", { name: NR_SHARED.REMOVE })).toBeDisabled();

    // Continuing without the task focuses it and says why, in the canon line.
    await next(page, NR_SHARED.STEP_FILES);
    await expect(page.locator("#nr-task")).toBeFocused();
    await expect(page.getByRole("alert").filter({ hasText: NR_HARNESSES.TASK_ERROR })).toBeVisible();
  });

  test("Interactive: two harnesses are picked and the start one is chosen", async ({ page }) => {
    const bodies = await stubFlow(page);
    await openGuided(page);
    await toHarnesses(page, "agent", "interactive");
    await expect(page.getByText(NR_HARNESSES.LEDE_INTERACTIVE)).toBeVisible();
    await press(page, NR_HARNESSES.ADD_ANOTHER);
    await expect(page.getByRole("region", { name: NR_HARNESSES.EYEBROW(2) })).toBeVisible();

    // Only the second one starts with the run, and only it takes the prompt.
    const second = page.getByRole("region", { name: NR_HARNESSES.EYEBROW(2) });
    await second.getByRole("switch", { name: NR_HARNESSES.START_IN_HARNESS }).click();
    await second.getByLabel(NR_HARNESSES.STARTING_PROMPT).fill("Review the diff");
    await expect(page.getByLabel(NR_HARNESSES.STARTING_PROMPT)).toHaveCount(1);
    await toReview(page);
    await launchThroughNameDialog(page, "guided two harnesses");
    expect(expectCreateEqualsLastCheck(bodies)).toMatchObject({
      experience: "interactive",
      startup: { kind: "harness", tool: CODEX.id, prompt: "Review the diff" },
    });
  });

  test("Interactive: Your own tools starts no CLI, so it offers no starting prompt", async ({ page }) => {
    await stubFlow(page);
    await openGuided(page);
    await toHarnesses(page, "agent", "interactive");
    await expect(page.getByLabel(NR_HARNESSES.STARTING_PROMPT)).toHaveCount(1);
    await page.locator("#nr-harness-0").selectOption({ label: "Your own tools" });
    await expect(page.getByLabel(NR_HARNESSES.STARTING_PROMPT)).toHaveCount(0);
  });

  test("a second harness no image carries shows its truthful disabled reason", async ({ page }) => {
    await stubFlow(page, { sets: [], harnesses: [CLAUDE, CODEX] });
    await openGuided(page);
    await toHarnesses(page, "agent", "interactive");
    await expect(page.getByRole("button", { name: NR_HARNESSES.ADD_ANOTHER })).toBeDisabled();
    await expect(page.getByText(NR_HARNESSES.NO_COMBINATION(CLAUDE.display, CODEX.display))).toBeVisible();
  });

  test("Your own tools brings no CLI, so it can join a harness even when no image carries a pair", async ({ page }) => {
    const [claude, , own] = SCENARIO_HARNESSES.full;
    await stubFlow(page, { harnesses: [claude, own], sets: [] });
    await openGuided(page);
    await toHarnesses(page, "agent", "interactive");
    await expect(page.getByRole("button", { name: NR_HARNESSES.ADD_ANOTHER })).toBeEnabled();
    await expect(page.getByText(NR_HARNESSES.NO_COMBINATION(CLAUDE.display, own.display))).toHaveCount(0);
    await press(page, NR_HARNESSES.ADD_ANOTHER);
    await expect(page.getByRole("region", { name: NR_HARNESSES.EYEBROW(2) })).toBeVisible();
  });

  test("a second harness a larger declared set carries is offered (DR-0225)", async ({ page }) => {
    const [claude, codex, own] = SCENARIO_HARNESSES.full;
    await stubFlow(page, { sets: [[claude.id, codex.id, own.id]] });
    await openGuided(page);
    await toHarnesses(page, "agent", "interactive");
    await expect(page.getByRole("button", { name: NR_HARNESSES.ADD_ANOTHER })).toBeEnabled();
    await expect(page.getByText(NR_HARNESSES.NO_COMBINATION(CLAUDE.display, CODEX.display))).toHaveCount(0);
    await press(page, NR_HARNESSES.ADD_ANOTHER);
    await expect(page.getByRole("region", { name: NR_HARNESSES.EYEBROW(2) })).toBeVisible();
  });

  test("not-installed, no-provider and not-enabled harnesses stay listed with their reasons", async ({ page }) => {
    const roster: SetupHarnessTool[] = [
      CLAUDE,
      { ...CODEX, installed: false },
      { ...CODEX, id: "custom-agent", display: "Custom agent", providers_ungranted: true },
      { ...CODEX, id: "off-agent", display: "Off agent", enabled: false },
    ];
    await stubFlow(page, { harnesses: roster });
    await openGuided(page);
    await toHarnesses(page, "agent", "interactive");
    const reasons = page.locator("#nr-harness-unavailable");
    await expect(reasons.getByText(NR_HARNESSES.NOT_INSTALLED(CODEX.display))).toBeVisible();
    await expect(reasons.getByText(NR_HARNESSES.NO_PROVIDER("Custom agent"))).toBeVisible();
    await expect(reasons.getByText(NR_HARNESSES.NOT_ENABLED)).toBeVisible();
  });
});

test.describe("New Run, Guided setup: leaving", () => {
  test("Esc and Runs ask before leaving a changed draft, and leave an untouched one", async ({ page }) => {
    await stubFlow(page);
    await openGuided(page);
    await press(page, NR_SHAPE.ENV_TITLE);
    await expect(page.getByRole("heading", { name: NR_RUN.TITLE_ENV })).toBeVisible();
    await page.keyboard.press("Escape");
    const dialog = page.getByRole("alertdialog", { name: UNSAVED_GUARD.TITLE });
    await expect(dialog).toBeVisible();
    await dialog.getByRole("button", { name: UNSAVED_GUARD.STAY }).click();
    await expect(dialog).toBeHidden();
    await expect(page.getByRole("heading", { name: NR_RUN.TITLE_ENV })).toBeVisible();

    await sidebarLink(page, "Runs").click();
    await expect(dialog).toBeVisible();
    await dialog.getByRole("button", { name: UNSAVED_GUARD.LEAVE }).click();
    // A closing dialog stays mounted through its exit animation and, still the top layer, takes the next Esc.
    await expect(dialog).toBeHidden();
    await expect(page).toHaveURL(/\/runs$/);
    // The address changes before the Runs screen replaces New Run (a route change renders a moment later),
    // and the draft goes with the screen. Coming back inside that moment would find New Run never left.
    await expect(page.getByRole("heading", { name: "Runs", level: 1 })).toBeVisible();

    // Untouched: opening Start and choosing Guided setup changes nothing, so Esc just leaves.
    await navToRoute(page, "/runs/new");
    await page.getByRole("button", { name: NR_START.GUIDED_FOOT }).click();
    await expect(page.getByRole("heading", { name: NR_SHAPE.TITLE })).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(page).toHaveURL(/\/runs$/);
  });
});
