/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New Run, step 5 Tools and access: the setup items the server says are
// missing are listed here and become blockers on Review, and the policy
// preview, the preflight and the create send the same body (DS-7 section 14).
//
// The preview is the real server's, with its `access` block replaced by the
// New Run scenario fixture; the preflight is stubbed from the same fixtures.
import type { Page } from "@playwright/test";
import { test, expect, gotoConsole, navToRoute } from "./fixtures";
import { NR_DOCUMENT } from "../src/app/components/wardyn/copy/new-run/document";
import { NR_LAUNCH } from "../src/app/components/wardyn/copy/new-run/launch";
import { NR_REVIEW } from "../src/app/components/wardyn/copy/new-run/review";
import { NR_SHARED } from "../src/app/components/wardyn/copy/new-run/shared";
import { NR_START } from "../src/app/components/wardyn/copy/new-run/start";
import { NR_TOOLS } from "../src/app/components/wardyn/copy/new-run/tools";
import { SCENARIO_HARNESSES, SCENARIO_HARNESS_SETS } from "../src/app/lib/new-run-fixtures/scenarios/harnesses";
import { SCENARIO_PREFLIGHT } from "../src/app/lib/new-run-fixtures/scenarios/preflight";
import { SCENARIO_ACCESS, SETUP_ITEMS } from "../src/app/lib/new-run-fixtures/scenarios/tools";

const RUN_ID = "d4444444-4444-4444-8444-444444444444";
const DOCUMENT = "api_version: wardyn/v1\nkind: Run\nintent:\n  shape: application\n  experience: background\n  workload:\n    kind: command\n    command: npm test\n  no_repositories_or_drives: true\n";
const SECRET = SETUP_ITEMS.addSecret.label;

const press = (page: Page, name: string) => page.getByRole("button", { name, exact: true }).click();
const launch = (page: Page) => page.getByRole("button", { name: NR_REVIEW.LAUNCH_RUN });

type Bodies = { preflight: string[]; preview: string[]; create: string[] };

/** `missing` makes the secret an unmet setup item in the preview's access block and the preflight. */
async function stub(page: Page, missing: boolean): Promise<Bodies> {
  const bodies: Bodies = { preflight: [], preview: [], create: [] };
  page.on("request", (req) => {
    if (req.method() !== "POST") return;
    const path = new URL(req.url()).pathname;
    if (path === "/api/v1/runs/preflight") bodies.preflight.push(req.postData() ?? "");
    else if (path === "/api/v1/runs/policy-preview") bodies.preview.push(req.postData() ?? "");
    else if (path === "/api/v1/runs") bodies.create.push(req.postData() ?? "");
  });
  const access = missing ? SCENARIO_ACCESS.app : { ...SCENARIO_ACCESS.app, setup: [] };
  await page.route("**/api/v1/runs/policy-preview", async (route) => {
    const response = await route.fetch();
    await route.fulfill({ response, json: { ...(await response.json()), access } });
  });
  await page.route("**/api/v1/runs/preflight", (route) =>
    route.fulfill({ json: missing ? SCENARIO_PREFLIGHT.app : SCENARIO_PREFLIGHT.member }),
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
  await page.route(`**/api/v1/runs/${RUN_ID}`, (route) => route.fulfill({ json: { id: RUN_ID, state: "PENDING" } }));
  await page.route("**/api/v1/runs", (route) =>
    route.request().method() === "POST" ? route.fulfill({ status: 201, json: { id: RUN_ID, state: "PENDING" } }) : route.fallback(),
  );
  return bodies;
}

/** Start, the document path to Review with a complete application draft, then Back to step 5. */
async function openTools(page: Page) {
  await gotoConsole(page);
  await navToRoute(page, "/runs/new");
  await press(page, NR_START.DOC_YAML);
  await page.getByRole("textbox", { name: NR_DOCUMENT.EDITOR_ARIA("YAML") }).fill(DOCUMENT);
  await press(page, NR_DOCUMENT.CONTINUE_REVIEW);
  await expect(page.getByRole("heading", { name: NR_REVIEW.TITLE })).toBeVisible();
  await press(page, NR_SHARED.BACK);
  await expect(page.getByRole("heading", { name: NR_TOOLS.TITLE })).toBeVisible();
}

test.describe("New Run, step 5: setup items and blockers", () => {
  test("a missing item is listed under Setup still needed and counted; Review turns it into a blocker", async ({ page }) => {
    await stub(page, true);
    await openTools(page);
    const setup = page.getByRole("region", { name: NR_TOOLS.SETUP_TITLE });
    await expect(setup.getByText(SECRET)).toBeVisible();
    await expect(page.getByText(NR_TOOLS.STICKY_ONE)).toBeVisible();

    // Reviewing now is allowed: the same item is a blocker there, and it links back here.
    await press(page, NR_TOOLS.REVIEW_RUN);
    await expect(page.getByText(NR_REVIEW.BLOCKERS_ONE)).toBeVisible();
    await expect(launch(page)).toBeDisabled();
    await page.locator("#nr-blockers").getByRole("button", { name: SECRET }).click();
    await expect(page.getByRole("heading", { name: NR_TOOLS.TITLE })).toBeVisible();
  });

  test("with nothing missing the step says everything is set up and Review has no blocker", async ({ page }) => {
    await stub(page, false);
    await openTools(page);
    await expect(page.getByText(NR_TOOLS.SETUP_ALL_SET)).toBeVisible();
    await expect(page.getByText(NR_TOOLS.STICKY_DONE)).toBeVisible();
    await press(page, NR_TOOLS.REVIEW_RUN);
    await expect(page.getByText(NR_REVIEW.BLOCKERS_ONE)).toHaveCount(0);
    await expect(launch(page)).toBeEnabled();
  });

  test("the policy preview, the preflight and the create send one body", async ({ page }) => {
    const bodies = await stub(page, false);
    await openTools(page);
    await press(page, NR_TOOLS.REVIEW_RUN);
    await expect(launch(page)).toBeEnabled();
    await launch(page).click();
    const dialog = page.getByRole("dialog", { name: NR_LAUNCH.TITLE });
    await dialog.getByLabel(NR_LAUNCH.TITLE_LABEL).fill("step five");
    await dialog.getByLabel(NR_LAUNCH.DESCRIPTION_LABEL).fill("same bytes");
    await dialog.getByRole("button", { name: NR_LAUNCH.LAUNCH, exact: true }).click();
    await expect(page).toHaveURL(new RegExp(`launched=${RUN_ID}`));

    expect(bodies.create).toHaveLength(1);
    const { title, description, ...rest } = JSON.parse(bodies.create[0]);
    expect([title, description]).toEqual(["step five", "same bytes"]);
    // Key order included: all three come out of one serializer.
    expect(JSON.stringify(rest)).toBe(JSON.stringify(JSON.parse(bodies.preview.at(-1) ?? "null")));
    expect(JSON.stringify(rest)).toBe(JSON.stringify(JSON.parse(bodies.preflight.at(-1) ?? "null")));
    expect(rest).toMatchObject({ mode_contract: "0.9", shape: "application", workload: { kind: "command", command: "npm test" } });
  });
});
