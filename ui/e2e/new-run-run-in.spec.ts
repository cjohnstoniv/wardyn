/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New Run, Run in: the unit a run runs in. The seeded people hold one unit and no groups, so the unit list and
// the person's requests are spliced here; the pools and the preflight come from the scenario fixtures.
import type { Page } from "@playwright/test";
import { test, expect, chooseFiles, chooseShape, continueNewRun, gotoConsole, navToRoute, startGuided } from "./fixtures";
import { ELEV } from "../src/app/components/wardyn/copy/elevation";
import { NR_FILES } from "../src/app/components/wardyn/copy/new-run/files";
import { NR_HARNESSES } from "../src/app/components/wardyn/copy/new-run/harnesses";
import { NR_REVIEW } from "../src/app/components/wardyn/copy/new-run/review";
import { NR_RUN } from "../src/app/components/wardyn/copy/new-run/run";
import { NR_RUN_IN } from "../src/app/components/wardyn/copy/new-run/run-in";
import { NR_SHARED } from "../src/app/components/wardyn/copy/new-run/shared";
import { NR_TOOLS } from "../src/app/components/wardyn/copy/new-run/tools";
import { SCENARIO_HARNESSES, SCENARIO_HARNESS_SETS } from "../src/app/lib/new-run-fixtures/scenarios/harnesses";
import { SCENARIO_POOLS } from "../src/app/lib/new-run-fixtures/scenarios/pools";
import { SCENARIO_PREFLIGHT } from "../src/app/lib/new-run-fixtures/scenarios/preflight";

const [SHARED] = SCENARIO_POOLS.admin.pools;
const REFUSAL = "The unit you asked to run in is not one of your units — name a unit you are a member of, or leave it out to run in your home unit";

const PLATFORM = { unit_id: "u-plat", unit_name: "Platform", home: true };
const DATA = { unit_id: "u-data", unit_name: "Data science", home: false };
const PAYMENTS = { unit_id: "u-pay", unit_name: "Payments", home: false };
const BILLING = { unit_id: "u-bill", unit_name: "Billing", max_elevation: "4h" };

const inHours = (h: number) => new Date(Date.now() + h * 3_600_000).toISOString();
const elevation = (over: Record<string, unknown> = {}) => ({
  id: "r-pay", target_kind: "governance_membership", op: "create", target_key: "u-pay", state: "applied", proposed_by: "e2e-member",
  proposed_at: inHours(-1), expires_at: inHours(23), diff: { changed: [] }, kind: "elevation", unit_id: "u-pay", unit_name: "Payments",
  duration: "4h", ends_at: inHours(3), mine: true, decidable: false, deciders: [{ kind: "security" }], ...over,
});
const pending = (over: Record<string, unknown> = {}) => elevation({ id: "r-bill", state: "pending", unit_id: "u-bill", unit_name: "Billing", ends_at: undefined, ...over });

/** What the console reads: the person's units and requests. Change it and the next read sees the change. */
interface World {
  contexts: object[];
  requests: object[];
  offered: object[];
  policyStatus: number;
  preflight: { status: number; body: object };
}

interface Seen {
  preflight: Record<string, unknown>[];
  created: Record<string, unknown>[];
}

async function stub(page: Page, world: World): Promise<Seen> {
  const seen: Seen = { preflight: [], created: [] };
  const pools = SCENARIO_POOLS.admin.pools.map((p) => ({
    ...p,
    run_types: ["background", "interactive"],
    run_types_source: "pool",
    barrier_options: [{ class: "CC1", available: true, default: true, default_source: "server" }],
  }));
  await page.route("**/api/v1/runner-pools*", (route) =>
    route.fulfill({ json: { pools, allowance: { active: 0, max: 5, unit_name: "Platform" }, default: { pool_id: SHARED.id, selection: "organisation_default" } } }),
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
  await page.route("**/api/v1/policies/default*", async (route) => {
    if (world.policyStatus !== 200) return route.fulfill({ status: world.policyStatus, json: { error: "unavailable" } });
    const real = (await (await route.fetch()).json()) as Record<string, unknown>;
    return route.fulfill({ json: { ...real, contexts: world.contexts } });
  });
  await page.route("**/api/v1/governance/requests", (route) => {
    if (route.request().method() === "POST") {
      const body = route.request().postDataJSON() as Record<string, unknown>;
      seen.created.push(body);
      return route.fulfill({ status: 202, json: { pending_change: { id: "r-bill" }, request: pending() } });
    }
    return route.fulfill({ json: { requests: world.requests, offered: world.offered, image_offered: false } });
  });
  await page.route("**/api/v1/runs/preflight", (route) => {
    seen.preflight.push(route.request().postDataJSON() as Record<string, unknown>);
    return route.fulfill({ status: world.preflight.status, json: world.preflight.body });
  });
  return seen;
}

const world = (over: Partial<World> = {}): World => ({
  contexts: [PLATFORM, DATA, PAYMENTS],
  requests: [elevation()],
  offered: [BILLING],
  policyStatus: 200,
  preflight: { status: 200, body: SCENARIO_PREFLIGHT.member },
  ...over,
});

const trigger = (page: Page) => page.getByRole("combobox", { name: NR_RUN_IN.LABEL });
async function pick(page: Page, name: RegExp) {
  await trigger(page).click();
  await page.getByRole("option", { name }).click();
}

/** The Run step of a new environment. */
async function openRunStep(page: Page) {
  await gotoConsole(page);
  await navToRoute(page, "/runs/new");
  await startGuided(page);
  await chooseShape(page, "environment");
}

/** On the Run step: through Harnesses, Files and Tools to Review. */
async function toReview(page: Page) {
  await continueNewRun(page, NR_HARNESSES.TITLE);
  await continueNewRun(page, NR_FILES.TITLE);
  await chooseFiles(page);
  await continueNewRun(page, NR_TOOLS.TITLE);
  await continueNewRun(page, NR_REVIEW.TITLE);
}

const blockers = (page: Page) => page.locator("#nr-blockers");

test.describe("New Run, Run in", () => {
  test("one unit and nothing offered: no Run in question", async ({ page }) => {
    await openRunStep(page);
    await expect(page.getByRole("combobox", { name: NR_RUN.POOL_LABEL })).toBeEnabled();
    await expect(page.getByText(NR_RUN_IN.QUESTION)).toHaveCount(0);
  });

  test("the choice lists your units, elevations with their end, and units you can ask for", async ({ page }) => {
    await stub(page, world());
    await openRunStep(page);
    await expect(trigger(page)).toHaveText(NR_RUN_IN.HOME_VALUE("Platform"));
    await expect(page.getByText(NR_RUN_IN.HINT)).toBeVisible();
    await trigger(page).click();
    for (const g of [NR_RUN_IN.GROUP_YOURS, NR_RUN_IN.GROUP_ELEVATED, NR_RUN_IN.GROUP_ASK]) await expect(page.getByText(g, { exact: true })).toBeVisible();
    const paid = page.getByRole("option", { name: /^Payments/ });
    await expect(paid.getByText(NR_RUN_IN.CHIP_ELEVATED)).toBeVisible();
    await expect(paid).toContainText(/Ends .+ \(in (2 h 5\d min|3 h)\)/);
    const bill = page.getByRole("option", { name: /^Billing/ });
    await expect(bill.getByText(NR_RUN_IN.CHIP_NEEDS)).toBeVisible();
    await expect(bill.getByText(NR_RUN_IN.ASK_LINE("4 hours"))).toBeVisible();
    await expect(page.getByRole("option", { name: /^Platform/ }).getByText(NR_RUN_IN.CHIP_HOME)).toBeVisible();
  });

  test("an elevated unit picked shows the Elevated chip, and the pool line names it", async ({ page }) => {
    const seen = await stub(page, world());
    await openRunStep(page);
    await pick(page, /^Payments/);
    await expect(trigger(page)).toHaveText(/^Payments/);
    await expect(page.getByText(NR_RUN_IN.CHIP_ELEVATED, { exact: true })).toBeVisible();
    await expect(page.getByText(NR_RUN_IN.HINT)).toHaveCount(0);
    await expect(page.getByText(NR_RUN.LIMITS_NOTE("Payments"))).toBeVisible();
    const asked = page.waitForRequest((r) => r.url().endsWith("/runs/preflight") && (r.postDataJSON() as { run_in?: string }).run_in === "u-pay");
    await toReview(page);
    await asked;
    expect(seen.preflight.at(-1)).toMatchObject({ run_in: "u-pay" });
    await expect(page.getByText(NR_RUN_IN.REVIEW_ROW("Payments"))).toBeVisible();
  });

  test("an offered unit picked: Review shows Request elevation, which opens the dialog with that unit", async ({ page }) => {
    await stub(page, world());
    await openRunStep(page);
    await pick(page, /^Billing/);
    await toReview(page);
    await expect(page.getByRole("button", { name: NR_REVIEW.LAUNCH_RUN })).toBeDisabled();
    await expect(blockers(page).getByText(NR_RUN_IN.BLOCKER_ASK("Billing"))).toBeVisible();
    await blockers(page).getByRole("button", { name: NR_RUN_IN.REQUEST_ELEVATION }).click();
    const dialog = page.getByRole("dialog", { name: ELEV.TITLE });
    await expect(dialog.getByRole("combobox", { name: ELEV.UNIT_LABEL })).toHaveText(/Billing/);
  });

  test("after sending, the blocker says the request waits", async ({ page }) => {
    const w = world();
    const seen = await stub(page, w);
    await openRunStep(page);
    await pick(page, /^Billing/);
    await toReview(page);
    await blockers(page).getByRole("button", { name: NR_RUN_IN.REQUEST_ELEVATION }).click();
    const dialog = page.getByRole("dialog", { name: ELEV.TITLE });
    await dialog.getByLabel(ELEV.REASON_LABEL).fill("Month-end close");
    w.requests = [elevation(), pending()];
    await dialog.getByRole("button", { name: ELEV.SEND }).click();
    await expect(dialog.getByText(ELEV.SENT_TITLE)).toBeVisible();
    await dialog.getByRole("button", { name: ELEV.DONE }).click();
    expect(seen.created).toEqual([expect.objectContaining({ kind: "elevation", unit_id: "u-bill" })]);
    await expect(blockers(page).getByText(NR_RUN_IN.BLOCKER_WAITING("Billing"))).toBeVisible();
    await expect(blockers(page).getByRole("button", { name: NR_RUN_IN.REQUEST_ELEVATION })).toHaveCount(0);
  });

  test("the server's own refusal shows its sentence", async ({ page }) => {
    await stub(page, world({ preflight: { status: 403, body: { error: REFUSAL, reason: "unit_not_a_context" } } }));
    await openRunStep(page);
    await pick(page, /^Payments/);
    await toReview(page);
    await expect(blockers(page).getByText(REFUSAL)).toBeVisible();
    await expect(blockers(page).getByRole("button", { name: NR_RUN_IN.REQUEST_ELEVATION })).toHaveCount(0);
  });

  test("a picked unit that left the list: the ended and the no-longer-in notices", async ({ page }) => {
    const w = world();
    await stub(page, w);
    await openRunStep(page);
    await pick(page, /^Payments/);
    await continueNewRun(page, NR_HARNESSES.TITLE);
    // Payments is gone from the units, and its request has lapsed.
    w.contexts = [PLATFORM, DATA];
    w.requests = [elevation({ ends_at: inHours(-1) })];
    await page.getByRole("button", { name: NR_SHARED.BACK, exact: true }).click();
    await expect(page.getByText(NR_RUN_IN.ENDED("Payments", "Platform"))).toBeVisible();
    await expect(trigger(page)).toHaveText(NR_RUN_IN.HOME_VALUE("Platform"));

    // A permanent unit the next read drops, with no request for it.
    await pick(page, /^Data science/);
    await continueNewRun(page, NR_HARNESSES.TITLE);
    w.contexts = [PLATFORM, PAYMENTS];
    await page.getByRole("button", { name: NR_SHARED.BACK, exact: true }).click();
    await expect(page.getByText(NR_RUN_IN.NO_LONGER_IN("Data science", "Platform"))).toBeVisible();
    await expect(trigger(page)).toHaveText(NR_RUN_IN.HOME_VALUE("Platform"));
  });

  test("the units could not be read", async ({ page }) => {
    const w = world();
    await stub(page, w);
    await openRunStep(page);
    await pick(page, /^Payments/);
    await continueNewRun(page, NR_HARNESSES.TITLE);
    w.policyStatus = 500;
    await page.getByRole("button", { name: NR_SHARED.BACK, exact: true }).click();
    await expect(trigger(page)).toBeDisabled();
    await expect(trigger(page)).toHaveText(NR_RUN_IN.HOME_VALUE("Platform"));
    await expect(page.getByText(NR_RUN_IN.UNITS_UNREADABLE)).toBeVisible();
  });
});
