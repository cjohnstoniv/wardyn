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
import { NR_START } from "../src/app/components/wardyn/copy/new-run/start";
import { NR_TOOLS } from "../src/app/components/wardyn/copy/new-run/tools";
import { SCENARIO_HARNESSES, SCENARIO_HARNESS_SETS } from "../src/app/lib/new-run-fixtures/scenarios/harnesses";
import { SCENARIO_PREFLIGHT } from "../src/app/lib/new-run-fixtures/scenarios/preflight";
import { SCENARIO_ACCESS, SETUP_ITEMS } from "../src/app/lib/new-run-fixtures/scenarios/tools";
import { CLAUDE_ATTENDED, CLAUDE_UNGATED, MCP_DOCS, withFacts } from "../src/app/lib/new-run-fixtures/scenarios/tools-harness";
import { MY_COMPONENTS } from "../src/app/components/wardyn/copy/components";
import { MY_COMPONENTS as CATALOGUE, ORG_LINEAR } from "../src/app/lib/new-run-fixtures/scenarios/catalogue";

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

/** Start, the document path to Review with a complete application draft, then Access's own way back to step 5. */
async function openTools(page: Page) {
  await gotoConsole(page);
  await navToRoute(page, "/runs/new");
  await press(page, NR_START.DOC_YAML);
  await page.getByRole("textbox", { name: NR_DOCUMENT.EDITOR_ARIA("YAML") }).fill(DOCUMENT);
  await press(page, NR_DOCUMENT.CONTINUE_REVIEW);
  await expect(page.getByRole("heading", { name: NR_REVIEW.TITLE })).toBeVisible();
  // Review has no Back: a group's Change opens the way to the step that owns it.
  await press(page, `${NR_REVIEW.CHANGE} ${NR_REVIEW.GROUP_ACCESS}`);
  await press(page, NR_REVIEW.EDIT_TOOLS_ACCESS);
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

const ENV_CLAUDE = ["shape: environment", "experience: interactive", "no_repositories_or_drives: true", "tools:", "  - id: claude-code", "    kind: harness"];

/** Like openTools, for a document of its own and the access facts the preview carries. */
async function openToolsFor(page: Page, intentLines: string[], access: object) {
  await stub(page, false);
  await page.route("**/api/v1/runs/policy-preview", async (route) => {
    const response = await route.fetch();
    await route.fulfill({ response, json: { ...(await response.json()), access } });
  });
  await gotoConsole(page);
  await navToRoute(page, "/runs/new");
  await press(page, NR_START.DOC_YAML);
  const text = ["api_version: wardyn/v1", "kind: Run", "intent:", ...intentLines.map((l) => `  ${l}`), ""].join("\n");
  await page.getByRole("textbox", { name: NR_DOCUMENT.EDITOR_ARIA("YAML") }).fill(text);
  await press(page, NR_DOCUMENT.CONTINUE_REVIEW);
  await expect(page.getByRole("heading", { name: NR_REVIEW.TITLE })).toBeVisible();
  await press(page, `${NR_REVIEW.CHANGE} ${NR_REVIEW.GROUP_ACCESS}`);
  await press(page, NR_REVIEW.EDIT_TOOLS_ACCESS);
  await expect(page.getByRole("heading", { name: NR_TOOLS.TITLE })).toBeVisible();
}
const card = (page: Page, name: string) => page.getByRole("region", { name, exact: true });

test.describe("New Run, step 5: one teal action", () => {
  test("a catalogue card's Add is an outline button; Review run is the one teal action (DR-0057)", async ({ page }) => {
    await stub(page, false);
    await page.route("**/api/v1/me/components", (route) => (route.request().method() === "GET" ? route.fulfill({ json: CATALOGUE }) : route.fallback()));
    await openTools(page);
    const add = page.getByRole("button", { name: `${NR_TOOLS.CARD_ADD} ${ORG_LINEAR.name}`, exact: true });
    await expect(add).toBeVisible();
    await expect(add).not.toHaveClass(/bg-primary/);
    await expect(page.getByRole("button", { name: NR_TOOLS.REVIEW_RUN, exact: true })).toHaveClass(/bg-primary/);
  });
});

test.describe("New Run, step 5: the harness card", () => {
  test("a harness that is only available is not labelled as starting with the run (DR-0230)", async ({ page }) => {
    await openToolsFor(page, ENV_CLAUDE, withFacts(SCENARIO_ACCESS.app, [CLAUDE_UNGATED]));
    await expect(card(page, "Claude Code").getByText(NR_TOOLS.SUB_AVAILABLE)).toBeVisible();
    await expect(card(page, "Claude Code").getByText(NR_TOOLS.SUB_STARTS)).toHaveCount(0);
  });

  test("a tool the policy allows by name can take its own rule under a blocking default (DR-0223)", async ({ page }) => {
    const refused = { effect: "allow", available: false, reason: "policy_stricter_tool" };
    const fact = {
      ...CLAUDE_ATTENDED,
      policy_rules: [{ tool: "Read", effect: "allow", source: { kind: "policy" } }],
      rule_effects: CLAUDE_ATTENDED.rule_effects.map((o) => (o.effect === "deny" ? o : { ...refused, effect: o.effect })),
    };
    await openToolsFor(page, ENV_CLAUDE, withFacts(SCENARIO_ACCESS.app, [fact]));
    await card(page, "Claude Code").getByRole("button", { name: NR_TOOLS.ADD_RULE }).click();
    const dialog = page.getByRole("dialog");
    await dialog.getByLabel(NR_TOOLS.RULE_TOOL).fill("Read");
    await expect(dialog.getByRole("radio", { name: NR_TOOLS.EFFECT_ALLOW })).toBeEnabled();
    await dialog.getByLabel(NR_TOOLS.RULE_TOOL).fill("Bash");
    await expect(dialog.getByRole("radio", { name: NR_TOOLS.EFFECT_ALLOW })).toBeDisabled();
  });

  test("an inline MCP server's per-harness toggle changes the draft (DR-0250)", async ({ page }) => {
    const row = { ...MCP_DOCS, id: "component:inline:0", source: { kind: "component", id: "inline:0", name: "Docs" } };
    const lines = [...ENV_CLAUDE, "components:", "  - name: Docs", "    inline:", "      hosts: [docs.example.com]", "      mcp:", "        url: https://docs.example.com/mcp", "        transport: http"];
    await openToolsFor(page, lines, withFacts(SCENARIO_ACCESS.app, [CLAUDE_UNGATED], [row]));
    const box = card(page, "Claude Code").getByRole("checkbox", { name: "Docs" });
    await expect(box).toBeChecked();
    await box.click();
    await expect(box).not.toBeChecked();
  });
});

test.describe("New Run, step 5: the Add dialogs", () => {
  test("a saved-secret read that fails says so and retries, and the dialog still adds without a token (DR-0252)", async ({ page }) => {
    let reads = 0;
    await page.route("**/api/v1/secrets*", (route) =>
      ++reads === 1 ? route.fulfill({ status: 500, json: { error: "secrets are offline" } }) : route.fulfill({ json: { names: ["linear-token"], mine: [] } }),
    );
    await stub(page, false);
    await openTools(page);
    await press(page, NR_TOOLS.MCP_CARD_BUTTON);
    const dialog = page.getByRole("dialog", { name: NR_TOOLS.MCP_CARD_TITLE });
    await expect(dialog.getByRole("alert")).toContainText("secrets are offline");
    await dialog.getByRole("button", { name: MY_COMPONENTS.RETRY, exact: true }).click();
    await expect(dialog.getByRole("alert")).toHaveCount(0);
    await expect(dialog.getByRole("option", { name: "linear-token" })).toHaveCount(1);
  });

  test("More options opens the full editor on what was typed and the save choice (DR-0251)", async ({ page }) => {
    await stub(page, false);
    await openTools(page);
    await press(page, NR_TOOLS.CUSTOM_COMPONENT);
    const dialog = page.getByRole("dialog", { name: NR_TOOLS.CUSTOM_COMPONENT });
    await dialog.getByLabel(NR_TOOLS.CC_NAME).fill("Billing");
    await dialog.getByLabel(NR_TOOLS.CC_HOSTS).fill("billing.acme.internal");
    await dialog.getByLabel(NR_TOOLS.CC_SAVE).check();
    await press(page, NR_TOOLS.CC_MORE_OPTIONS);
    await expect(page.locator("#ccd-name")).toHaveValue("Billing");
    await expect(page.locator("#ccd-hosts")).toHaveValue("billing.acme.internal");
  });
});
