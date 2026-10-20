/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New Run, Application / server (#1994): the Run step's second card with its start command and ports, the
// Review step's port rows, the gateway-off blocker, and the server's clamp warning when governance strips the
// apps. The pool list and the preflight are stubbed as in new-run-guided.spec.ts; the policy preview goes to
// the real server unless a case stubs it. /healthz is real (the seeded backend runs the gateway) unless a
// case turns it off.
import type { Page } from "@playwright/test";
import { test, expect, continueNewRun, fillMainDriveSize, gotoConsole, navToRoute } from "./fixtures";
import { NR_FILES } from "../src/app/components/wardyn/copy/new-run/files";
import { NR_HARNESSES } from "../src/app/components/wardyn/copy/new-run/harnesses";
import { NR_LAUNCH } from "../src/app/components/wardyn/copy/new-run/launch";
import { NR_REVIEW } from "../src/app/components/wardyn/copy/new-run/review";
import { NR_RUN } from "../src/app/components/wardyn/copy/new-run/run";
import { NR_SHAPE } from "../src/app/components/wardyn/copy/new-run/shape";
import { NR_SHARED } from "../src/app/components/wardyn/copy/new-run/shared";
import { NR_START } from "../src/app/components/wardyn/copy/new-run/start";
import { NR_TOOLS } from "../src/app/components/wardyn/copy/new-run/tools";
import { SCENARIO_POOLS } from "../src/app/lib/new-run-fixtures/scenarios/pools";
import { SCENARIO_PREFLIGHT } from "../src/app/lib/new-run-fixtures/scenarios/preflight";

const RUN_ID = "a2222222-2222-4222-8222-222222222222";

/** Stubs the pool list and preflight, and records the bodies the flow sends. */
async function stubFlow(page: Page, warnings: string[] = []) {
  const bodies = { preflight: [] as string[], create: [] as string[] };
  page.on("request", (req) => {
    if (req.method() !== "POST") return;
    const path = new URL(req.url()).pathname;
    if (path === "/api/v1/runs/preflight") bodies.preflight.push(req.postData() ?? "");
    else if (path === "/api/v1/runs") bodies.create.push(req.postData() ?? "");
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
  await page.route("**/api/v1/runs/preflight", (route) => route.fulfill({ json: { ...SCENARIO_PREFLIGHT.member, warnings } }));
  await page.route("**/api/v1/runs", (route) =>
    route.request().method() === "POST"
      ? route.fulfill({ status: 201, json: { id: RUN_ID, state: "PENDING" } })
      : route.fallback(),
  );
  await page.route(`**/api/v1/runs/${RUN_ID}`, (route) => route.fulfill({ json: { id: RUN_ID, state: "PENDING" } }));
  return bodies;
}

async function toShape(page: Page) {
  await gotoConsole(page);
  await navToRoute(page, "/runs/new");
  await page.getByRole("button", { name: NR_START.GUIDED_FOOT }).click();
}

/** Shape, then the Application / server card on the Run step. */
async function toServerCard(page: Page) {
  await toShape(page);
  await pickServer(page);
}

async function pickServer(page: Page) {
  await page.getByRole("button", { name: NR_SHAPE.APP_TITLE, exact: true }).click();
  await expect(page.getByRole("heading", { name: NR_RUN.TITLE_APP, level: 1 })).toBeVisible();
  await page.getByRole("radio", { name: NR_RUN.APP_SERVER }).click();
  await expect(page.getByRole("heading", { name: NR_RUN.TITLE_APP_SERVER, level: 1 })).toBeVisible();
}

const row = (page: Page, i: number) => page.locator(`[data-app-row="${i}"]`);

/** Files and Tools with nothing extra, then Review. */
async function toReview(page: Page) {
  await continueNewRun(page, NR_HARNESSES.TITLE);
  await continueNewRun(page, NR_FILES.TITLE);
  await page.getByRole("checkbox", { name: NR_FILES.NO_EXTRA }).check();
  await fillMainDriveSize(page);
  await continueNewRun(page, NR_TOOLS.TITLE);
  await continueNewRun(page, NR_REVIEW.TITLE);
}

test.describe("New Run: Application / server, the Run step", () => {
  test("offers both cards as available now and shows the server's start command and ports", async ({ page }) => {
    await stubFlow(page);
    await toShape(page);
    await expect(page.getByText(NR_SHAPE.APP_SUB)).toBeVisible();
    await pickServer(page);
    const server = page.getByRole("radio", { name: NR_RUN.APP_SERVER });
    await expect(server).toBeChecked();
    await expect(page.getByText(NR_RUN.APP_SERVER_BODY)).toBeVisible();
    await expect(page.getByText(NR_RUN.APP_AUTOMATION_META)).toHaveCount(2);
    await expect(page.getByText(NR_SHARED.COMING_SOON)).toHaveCount(0);
    await expect(page.getByLabel(new RegExp(NR_RUN.START_COMMAND_LABEL))).toBeVisible();
    await expect(page.getByText(NR_RUN.START_COMMAND_HELP)).toBeVisible();
    await expect(page.getByText(NR_RUN.PORTS_HELP)).toBeVisible();
    await expect(page.getByText(NR_RUN.APP_TERMINAL_NOTE)).toBeVisible();
    await expect(row(page, 0).getByLabel(NR_RUN.PORT_COL_PORT)).toHaveValue("3000");
    // Back to the script card: the ports and the terminal note go, the command's own words return.
    await page.getByRole("radio", { name: NR_RUN.APP_AUTOMATION }).click();
    await expect(page.getByText(NR_RUN.COMMAND_HELP)).toBeVisible();
    await expect(page.getByText(NR_RUN.PORTS_LABEL)).toHaveCount(0);
    await expect(page.getByRole("heading", { name: NR_RUN.TITLE_APP, level: 1 })).toBeVisible();
  });

  test("says a bad or shared port on its row, keeps Continue shut, and stops at eight ports", async ({ page }) => {
    await stubFlow(page);
    await toServerCard(page);
    await page.getByLabel(new RegExp(NR_RUN.START_COMMAND_LABEL)).fill("npm run dev");
    await page.getByRole("button", { name: NR_RUN.PORT_ADD }).click();
    await expect(row(page, 1).getByRole("alert")).toHaveText(NR_RUN.PORT_INVALID);
    await row(page, 1).getByLabel(NR_RUN.PORT_COL_PORT).fill("3000");
    await expect(row(page, 1).getByRole("alert")).toHaveText(NR_RUN.PORT_DUPLICATE(3000));
    await row(page, 1).getByLabel(NR_RUN.PORT_COL_PORT).fill("70000");
    await expect(row(page, 1).getByRole("alert")).toHaveText(NR_RUN.PORT_INVALID);
    await page.getByRole("button", { name: new RegExp(`^${NR_SHARED.CONTINUE_TO("")}`) }).click();
    await expect(row(page, 1).getByLabel(NR_RUN.PORT_COL_PORT)).toBeFocused();
    await expect(page.getByRole("heading", { name: NR_RUN.TITLE_APP_SERVER, level: 1 })).toBeVisible();
    await row(page, 1).getByRole("button", { name: NR_SHARED.REMOVE }).click();
    await expect(page.locator("[data-app-row]")).toHaveCount(1);
    await row(page, 0).getByRole("button", { name: NR_SHARED.REMOVE }).click();
    await expect(page.getByText(NR_RUN.PORT_REQUIRED)).toBeVisible();
    await page.getByRole("button", { name: NR_RUN.PORT_ADD }).click();
    await row(page, 0).getByLabel(NR_RUN.PORT_COL_PORT).fill("3000");
    for (let n = 0; n < 7; n++) await page.getByRole("button", { name: NR_RUN.PORT_ADD }).click();
    await expect(page.locator("[data-app-row]")).toHaveCount(8);
    await expect(page.getByRole("button", { name: NR_RUN.PORT_ADD })).toBeDisabled();
    await expect(page.getByText(NR_RUN.PORTS_FULL)).toBeVisible();
  });
});

test.describe("New Run: Application / server, Review and launch", () => {
  test("names each port on Review and creates an interactive run that starts the command and declares the ports", async ({ page }) => {
    const bodies = await stubFlow(page);
    await toServerCard(page);
    await page.getByLabel(new RegExp(NR_RUN.START_COMMAND_LABEL)).fill("npm run dev");
    await page.getByRole("button", { name: NR_RUN.PORT_ADD }).click();
    await row(page, 1).getByLabel(NR_RUN.PORT_COL_NAME).fill("api");
    await row(page, 1).getByLabel(NR_RUN.PORT_COL_PORT).fill("8080");
    await row(page, 1).getByLabel(NR_RUN.PORT_COL_PATH).fill("/api");
    await toReview(page);

    await expect(page.getByText(NR_REVIEW.SETUP_APPLICATION_SERVER, { exact: true })).toBeVisible();
    const run = page.locator("[data-review-group=run]");
    await expect(run.getByText(NR_REVIEW.TIMING_APPLICATION_SERVER)).toBeVisible();
    await expect(run.locator('[data-app="web"]')).toContainText(`${NR_REVIEW.APP_PORT(3000)}${NR_REVIEW.APP_OPENS("/")}`);
    await expect(run.locator('[data-app="api"]')).toContainText(`${NR_REVIEW.APP_PORT(8080)}${NR_REVIEW.APP_OPENS("/api")}`);
    const tools = page.locator("[data-review-group=tools]");
    await expect(tools.getByText("npm run dev")).toBeVisible();
    await expect(tools.getByText(NR_REVIEW.TOOLS_START_COMMAND)).toBeVisible();
    await expect(page.getByRole("button", { name: NR_REVIEW.LAUNCH_RUN })).toBeEnabled();

    await page.getByRole("button", { name: NR_REVIEW.LAUNCH_RUN }).click();
    const dialog = page.getByRole("dialog", { name: NR_LAUNCH.TITLE });
    await dialog.getByLabel(NR_LAUNCH.TITLE_LABEL).fill("dev server");
    await dialog.getByRole("button", { name: NR_LAUNCH.LAUNCH, exact: true }).click();
    await expect(page).toHaveURL(new RegExp(`launched=${RUN_ID}`));
    expect(bodies.create).toHaveLength(1);
    const { title, description, ...create } = JSON.parse(bodies.create[0]);
    void title;
    void description;
    expect(create).toEqual(JSON.parse(bodies.preflight.at(-1) ?? "null"));
    expect(create).toMatchObject({
      shape: "application",
      experience: "interactive",
      startup: { kind: "command", command: "npm run dev" },
      policy_overlay: { ui_apps: [{ name: "web", port: 3000, path: "/" }, { name: "api", port: 8080, path: "/api" }] },
    });
    expect(create).not.toHaveProperty("workload");
  });

  test("holds Launch with one blocker, linking to the Run step, when the web app gateway is off", async ({ page }) => {
    await stubFlow(page);
    await page.route("**/healthz", (route) => route.fulfill({ json: { status: "ok" } }));
    await toServerCard(page);
    await page.getByLabel(new RegExp(NR_RUN.START_COMMAND_LABEL)).fill("npm run dev");
    await toReview(page);
    const blockers = page.locator("#nr-blockers");
    await expect(blockers).toContainText(NR_REVIEW.BLOCKERS_ONE);
    await expect(blockers.getByRole("listitem")).toHaveText(NR_REVIEW.BLOCKER_GATEWAY_OFF);
    await expect(page.getByRole("button", { name: NR_REVIEW.LAUNCH_RUN })).toBeDisabled();
    await expect(page.locator("[data-review-group=run]").getByText(NR_REVIEW.BADGE_ATTENTION)).toBeAttached();
    await expect(page.locator("[data-review-group=tools]").getByText(NR_REVIEW.BADGE_ATTENTION)).toHaveCount(0);
    await blockers.getByRole("button", { name: NR_REVIEW.BLOCKER_GATEWAY_OFF }).click();
    await expect(page.getByRole("heading", { name: NR_RUN.TITLE_APP_SERVER, level: 1 })).toBeVisible();
  });

  test("shows the server's clamp warning and no port rows when governance strips the apps", async ({ page }) => {
    const warning = 'dropped 2 ui_app(s) denied by governance profile "Platform team": web:3000,api:8080';
    await stubFlow(page, [warning]);
    await page.route("**/api/v1/runs/policy-preview", async (route) => {
      const response = await route.fetch();
      if (!response.ok()) return route.fulfill({ response });
      const json = (await response.json()) as { spec?: Record<string, unknown> };
      delete json.spec?.ui_apps;
      await route.fulfill({ response, json });
    });
    await toServerCard(page);
    await page.getByLabel(new RegExp(NR_RUN.START_COMMAND_LABEL)).fill("npm run dev");
    await toReview(page);
    await expect(page.getByText(warning)).toBeVisible();
    await expect(page.locator("[data-app]")).toHaveCount(0);
    await expect(page.getByText(NR_REVIEW.APP_OPENS("/"))).toHaveCount(0);
  });
});
