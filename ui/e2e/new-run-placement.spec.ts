/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New Run, placement (DS-5 T24): a capped pool that is full tells Review, and
// Launch lands on Waiting where the run can be cancelled; an offline
// user pool is a blocker that links to the Run step; a pool the person
// picks reaches the wire, and an untouched one sends none.
//
// Pools and the preflight are stubbed from the New Run scenario fixtures: the
// seeded backend runs no runner and holds no pool.
import { RUN_REF } from "../src/app/components/wardyn/copy/run-ref";
import { shortRunId } from "../src/app/components/wardyn/run-ref";
import type { Page } from "@playwright/test";
import { test, expect, fillMainDriveSize, gotoConsole, navToRoute } from "./fixtures";
import { NR_DOCUMENT } from "../src/app/components/wardyn/copy/new-run/document";
import { NR_LAUNCH } from "../src/app/components/wardyn/copy/new-run/launch";
import { NR_REVIEW } from "../src/app/components/wardyn/copy/new-run/review";
import { NR_FILES } from "../src/app/components/wardyn/copy/new-run/files";
import { NR_HARNESSES } from "../src/app/components/wardyn/copy/new-run/harnesses";
import { NR_RUN } from "../src/app/components/wardyn/copy/new-run/run";
import { NR_SHAPE } from "../src/app/components/wardyn/copy/new-run/shape";
import { NR_SHARED } from "../src/app/components/wardyn/copy/new-run/shared";
import { NR_START } from "../src/app/components/wardyn/copy/new-run/start";
import { NR_TOOLS } from "../src/app/components/wardyn/copy/new-run/tools";
import { SCENARIO_HARNESSES, SCENARIO_HARNESS_SETS } from "../src/app/lib/new-run-fixtures/scenarios/harnesses";
import { SCENARIO_POOLS, effectiveLimits } from "../src/app/lib/new-run-fixtures/scenarios/pools";
import { SCENARIO_PREFLIGHT } from "../src/app/lib/new-run-fixtures/scenarios/preflight";
import { POOL_OFFLINE, RUNNER_POOL_REFUSAL, RUN_WAIT, RUN_WAIT_REASON } from "../src/app/lib/runner-pool-refusals";

const RUN_ID = "c3333333-3333-4333-8333-333333333333";
const FULL = SCENARIO_PREFLIGHT.full;
const [SHARED, LAPTOPS] = SCENARIO_POOLS.admin.pools;
const DOCUMENT = "api_version: wardyn/v1\nkind: Run\nintent:\n  shape: environment\n  experience: interactive\n  no_repositories_or_drives: true\n";

const press = (page: Page, name: string) => page.getByRole("button", { name, exact: true }).click();
const launch = (page: Page) => page.getByRole("button", { name: NR_REVIEW.LAUNCH_RUN });

interface Seen {
  preflight: string[];
  create: string[];
  killed: string[];
}

/** Pools, status and the run endpoints; a Waiting run when `queued`. */
async function stub(page: Page, opts: { preflight: object | { status: number; body: object }; queued?: boolean }): Promise<Seen> {
  const seen: Seen = { preflight: [], create: [], killed: [] };
  // The full pool is in the list too: the Launched screen reads a waiting run's pool from it.
  const ci = { ...SHARED, id: FULL.runner_pool!.id, name: FULL.runner_pool!.name, slots: { ...SHARED.slots!, used: 40, max: 40, verdict: "waits" as const } };
  const pools = [...SCENARIO_POOLS.admin.pools, ci].map((p) => ({
    ...p,
    run_types: ["background", "interactive"],
    run_types_source: "pool",
    barrier_options: [
      { class: "CC1", available: true, default: true, default_source: "server" },
      { class: "CC2", available: p.barriers?.includes("CC2") ?? false },
      { class: "CC3", available: p.barriers?.includes("CC3") ?? false },
    ],
  }));
  await page.route("**/api/v1/runner-pools*", (route) =>
    route.fulfill({ json: { pools, default: { pool_id: SHARED.id, selection: "organisation_default" } } }),
  );
  let status: Record<string, unknown> | null = null;
  await page.route("**/api/v1/setup/status*", async (route) => {
    status ??= {
      ...((await (await route.fetch()).json()) as Record<string, unknown>),
      harnesses: SCENARIO_HARNESSES.full,
      harness_sets: SCENARIO_HARNESS_SETS.full,
    };
    await route.fulfill({ json: status });
  });
  await page.route("**/api/v1/runs/preflight", (route) => {
    seen.preflight.push(route.request().postData() ?? "");
    const next = opts.preflight;
    return "status" in next ? route.fulfill({ status: next.status as number, json: (next as { body: object }).body }) : route.fulfill({ json: next });
  });
  const queue = { pool_id: ci.id, queued_at: new Date().toISOString(), deadline_at: new Date(Date.now() + 86_400_000).toISOString(), position: 1, reason: RUN_WAIT_REASON.AT_CAPACITY };
  await page.route(`**/api/v1/runs/${RUN_ID}`, (route) => route.fulfill({ json: { id: RUN_ID, state: opts.queued ? "QUEUED" : "PENDING", ...(opts.queued && { queue }) } }));
  await page.route(`**/api/v1/runs/${RUN_ID}/kill`, (route) => {
    seen.killed.push(RUN_ID);
    return route.fulfill({ status: 204, body: "" });
  });
  await page.route("**/api/v1/runs", (route) => {
    if (route.request().method() !== "POST") return route.fallback();
    seen.create.push(route.request().postData() ?? "");
    return route.fulfill({
      status: 201,
      json: { id: RUN_ID, state: opts.queued ? "QUEUED" : "PENDING", ...(opts.queued && { warnings: [RUN_WAIT.AT_CAPACITY("CI jobs", 40)] }) },
    });
  });
  return seen;
}

/** Start, then the document path to Review with a complete environment draft. */
async function openReview(page: Page) {
  await gotoConsole(page);
  await navToRoute(page, "/runs/new");
  await press(page, NR_START.DOC_YAML);
  await page.getByRole("textbox", { name: NR_DOCUMENT.EDITOR_ARIA("YAML") }).fill(DOCUMENT);
  await press(page, NR_DOCUMENT.CONTINUE_REVIEW);
  await expect(page.getByRole("heading", { name: NR_REVIEW.TITLE })).toBeVisible();
}

async function nameAndLaunch(page: Page, title: string) {
  await expect(launch(page)).toBeEnabled();
  await launch(page).click();
  const dialog = page.getByRole("dialog", { name: NR_LAUNCH.TITLE });
  await dialog.getByLabel(NR_LAUNCH.TITLE_LABEL).fill(title);
  await dialog.getByRole("button", { name: NR_LAUNCH.LAUNCH, exact: true }).click();
  await expect(page).toHaveURL(new RegExp(`launched=${RUN_ID}`));
}

test.describe("New Run, placement: a full pool and an offline pool", () => {
  test("a full capped pool is a notice on Review; Launch lands on Waiting; Cancel kills the waiting run", async ({ page }) => {
    const seen = await stub(page, { preflight: FULL, queued: true });
    await openReview(page);

    // Review says it plainly, and still lets the run launch: waiting is not a refusal.
    await expect(page.getByText(RUN_WAIT.AT_CAPACITY("CI jobs", 40))).toBeVisible();
    await expect(page.getByText(NR_REVIEW.WHERE_VALUE("CI jobs"), { exact: true })).toBeVisible();
    await expect(page.getByText(NR_REVIEW.WHERE_DETAIL("Organization pool", 40, 40))).toBeVisible();

    await nameAndLaunch(page, "waiting run");
    await expect(page.getByText(NR_LAUNCH.BADGE_WAITING, { exact: true })).toBeVisible();
    await expect(page.getByText(NR_LAUNCH.WAITING_BODY("CI jobs", 40))).toBeVisible();
    await expect(page.getByText(NR_LAUNCH.POSITION, { exact: true })).toBeVisible();
    // position is 1 on the wire; the console says the place in line, one more.
    await expect(page.getByText(NR_LAUNCH.IN_LINE("2nd"))).toBeVisible();
    await expect(page.getByText(NR_LAUNCH.WAITING_SINCE, { exact: true })).toBeVisible();
    await expect(page.getByText(NR_LAUNCH.GIVES_UP, { exact: true })).toBeVisible();

    // Cancel goes through the same kill dialog the Runs board uses.
    await press(page, NR_LAUNCH.CANCEL_RUN);
    const confirm = page.getByRole("alertdialog", { name: RUN_REF.killRun(shortRunId(RUN_ID)) });
    await expect(confirm).toBeVisible();
    await confirm.getByRole("button", { name: "Kill run" }).click();
    await expect.poll(() => seen.killed).toEqual([RUN_ID]);
  });

  test("an offline user pool is a blocker in the server's sentence, and it links to the Run step", async ({ page }) => {
    const sentence = POOL_OFFLINE.NO_ONLINE_RUNNER(LAPTOPS.name);
    await stub(page, { preflight: { status: 422, body: { error: sentence, reason: "runner_offline" } } });
    await openReview(page);
    await expect(launch(page)).toBeDisabled();
    const blocker = page.locator("#nr-blockers").getByRole("button", { name: sentence });
    await expect(blocker).toBeVisible();
    await blocker.click();
    await expect(page.getByRole("heading", { name: NR_RUN.TITLE_ENV })).toBeVisible();
    await expect(page).toHaveURL(/step=run/);
  });

  test("a model credential refusal keeps the setup blockers it carries, and Launch stays shut", async ({ page }) => {
    const [item] = SCENARIO_PREFLIGHT.app.setup_items ?? [];
    await stub(page, {
      preflight: {
        status: 422,
        body: { error: "Your AWS sign-in no longer works.", reason: "model_credential", provider: "bedrock-sso", setup_items: [item] },
      },
    });
    await openReview(page);
    await expect(launch(page)).toBeDisabled();
    await expect(page.locator("#nr-blockers")).toContainText(item.label);
  });
});

test.describe("New Run, placement: the pool a person picks", () => {
  /** Guided to Review for an environment, choosing `pool` on the Run step first when given. */
  async function guidedEnvironment(page: Page, pool?: string) {
    await gotoConsole(page);
    await navToRoute(page, "/runs/new");
    await page.getByRole("button", { name: NR_START.GUIDED_FOOT }).click();
    await press(page, NR_SHAPE.ENV_TITLE);
    if (pool) {
      await page.getByRole("combobox", { name: NR_RUN.POOL_LABEL }).selectOption({ label: pool });
    }
    await press(page, NR_SHARED.CONTINUE_TO(NR_SHARED.STEP_HARNESSES));
    await press(page, NR_SHARED.CONTINUE_TO(NR_SHARED.STEP_FILES));
    await page.getByRole("checkbox", { name: NR_FILES.NO_EXTRA }).check();
    await fillMainDriveSize(page);
    await press(page, NR_SHARED.CONTINUE_TO(NR_SHARED.STEP_TOOLS));
    await press(page, NR_TOOLS.REVIEW_RUN);
    await expect(page.getByRole("heading", { name: NR_REVIEW.TITLE })).toBeVisible();
  }

  test("each pool in the dropdown says whether a run starts now or waits for a slot", async ({ page }) => {
    await stub(page, { preflight: SCENARIO_PREFLIGHT.member });
    await gotoConsole(page);
    await navToRoute(page, "/runs/new");
    await page.getByRole("button", { name: NR_START.GUIDED_FOOT }).click();
    await press(page, NR_SHAPE.ENV_TITLE);
    await expect(page.getByRole("combobox", { name: NR_RUN.POOL_LABEL }).locator("option")).toHaveText([
      NR_RUN.POOL_OPTION_REASON(SHARED.name, NR_RUN.FIT_STARTS_NOW),
      NR_RUN.POOL_OPTION_REASON(LAPTOPS.name, NR_RUN.FIT_STARTS_NOW),
      NR_RUN.POOL_OPTION_REASON(FULL.runner_pool!.name, NR_RUN.FIT_WAITS),
    ]);
  });

  test("an untouched pool sends no runner_pool_id", async ({ page }) => {
    const seen = await stub(page, { preflight: SCENARIO_PREFLIGHT.member });
    await guidedEnvironment(page);
    await nameAndLaunch(page, "default pool");
    expect(JSON.parse(seen.create[0])).not.toHaveProperty("runner_pool_id");
    expect(JSON.parse(seen.preflight.at(-1)!)).not.toHaveProperty("runner_pool_id");
  });

  test("a picked pool is named in the check and in the create", async ({ page }) => {
    const seen = await stub(page, { preflight: SCENARIO_PREFLIGHT.member });
    await guidedEnvironment(page, NR_RUN.POOL_OPTION_REASON(LAPTOPS.name, NR_RUN.FIT_STARTS_NOW));
    await nameAndLaunch(page, "picked pool");
    expect(JSON.parse(seen.create[0])).toMatchObject({ runner_pool_id: LAPTOPS.id });
    expect(JSON.parse(seen.preflight.at(-1)!)).toMatchObject({ runner_pool_id: LAPTOPS.id });
  });
  test("a limit the deployment bounds says it is set by this server, and the meters carry their canon names", async ({ page }) => {
    await stub(page, { preflight: SCENARIO_PREFLIGHT.member });
    const bounded = { ...SHARED, limits: [effectiveLimits("background"), effectiveLimits("interactive", { memory_mib: { default: { value: 1024 }, cap: { value: 4096 }, cap_source: "deployment" } })] };
    await page.route("**/api/v1/runner-pools*", (route) =>
      route.fulfill({ json: { pools: [bounded], allowance: { active: 1, max: 5 }, default: { pool_id: SHARED.id, selection: "organisation_default" } } }),
    );
    await gotoConsole(page);
    await navToRoute(page, "/runs/new");
    await page.getByRole("button", { name: NR_START.GUIDED_FOOT }).click();
    await press(page, NR_SHAPE.ENV_TITLE);
    await expect(page.locator('[data-limit="memory"]').getByText(NR_RUN.SET_BY_SERVER)).toBeVisible();
    await expect(page.getByRole("meter", { name: NR_RUN.CAPACITY_ARIA })).toBeVisible();
    await expect(page.getByRole("meter", { name: NR_RUN.ALLOWANCE_ARIA })).toBeVisible();
  });

  test("a chosen pool the list no longer holds says so on the Run step", async ({ page }) => {
    await stub(page, { preflight: SCENARIO_PREFLIGHT.member });
    await gotoConsole(page);
    await navToRoute(page, "/runs/new");
    await page.getByRole("button", { name: NR_START.GUIDED_FOOT }).click();
    await press(page, NR_SHAPE.ENV_TITLE);
    await page.getByRole("combobox", { name: NR_RUN.POOL_LABEL }).selectOption({ label: NR_RUN.POOL_OPTION_REASON(LAPTOPS.name, NR_RUN.FIT_STARTS_NOW) });
    await press(page, NR_SHARED.CONTINUE_TO(NR_SHARED.STEP_HARNESSES));
    // Back is on the Run step too: wait for Harnesses, or the click can land before the step has changed.
    await expect(page.getByRole("heading", { name: NR_HARNESSES.TITLE })).toBeVisible();
    // The pool is gone from the list when the person comes back.
    await page.route("**/api/v1/runner-pools*", (route) =>
      route.fulfill({ json: { pools: [SHARED], default: { pool_id: SHARED.id, selection: "organisation_default" } } }),
    );
    await press(page, NR_SHARED.BACK);
    await expect(page.getByText(RUNNER_POOL_REFUSAL.NOT_FOUND())).toBeVisible();
  });
});
