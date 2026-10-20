/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New Run, step 5: an MCP server that runs in the sandbox (features.mcp_local_servers on
// /setup/status). Add an MCP server gets its two tabs, an added server says where it runs, an
// environment secret says the agent can read it, and Codex CLI under a rule it cannot apply is a
// Review blocker. With the switch off the dialog is today's.
//
// The policy preview is stubbed from the fixtures with an `access` block built from the body it was
// sent, so the rows are those of the component the person just added.
import type { Page } from "@playwright/test";
import { test, expect, gotoConsole, navToRoute } from "./fixtures";
import { NR_DOCUMENT } from "../src/app/components/wardyn/copy/new-run/document";
import { NR_REVIEW } from "../src/app/components/wardyn/copy/new-run/review";
import { NR_SHARED } from "../src/app/components/wardyn/copy/new-run/shared";
import { NR_START } from "../src/app/components/wardyn/copy/new-run/start";
import { NR_TOOLS } from "../src/app/components/wardyn/copy/new-run/tools";
import { preview } from "../src/app/lib/new-run-fixtures/base";
import { SCENARIO_HARNESSES, SCENARIO_HARNESS_SETS } from "../src/app/lib/new-run-fixtures/scenarios/harnesses";
import { SCENARIO_PREFLIGHT } from "../src/app/lib/new-run-fixtures/scenarios/preflight";
import { MY_COMPONENTS } from "../src/app/lib/new-run-fixtures/scenarios/catalogue";
import { SCENARIO_ACCESS } from "../src/app/lib/new-run-fixtures/scenarios/tools";
import { CLAUDE_UNGATED, CODEX, MCP_LOCAL, MCP_LOCAL_ENV } from "../src/app/lib/new-run-fixtures/scenarios/tools-harness";

const INTENT = ["shape: environment", "experience: interactive", "no_repositories_or_drives: true", "tools:", "  - id: claude-code", "    kind: harness", "  - id: codex-cli", "    kind: harness"];
const DOCUMENT = ["api_version: wardyn/v1", "kind: Run", "intent:", ...INTENT.map((l) => `  ${l}`), ""].join("\n");
const COMMAND = "npx -y @example/mcp-server@1.4.2";

const press = (page: Page, name: string) => page.getByRole("button", { name, exact: true }).click();
const card = (page: Page, name: string) => page.getByRole("region", { name, exact: true });

type Sent = { components?: { name?: string; mcp_harnesses?: string[]; inline?: { hosts?: string[]; secrets?: unknown[]; mcp?: { transport?: string; launch?: { command: string; args?: string[] } } } }[] };

/** The access block of a body: the base run, plus the local server it carries with the harnesses it is on for. */
function accessFor(raw: string | null) {
  const body = JSON.parse(raw ?? "{}") as Sent;
  const base = { ...SCENARIO_ACCESS.app, setup: [], harnesses: [CLAUDE_UNGATED, CODEX] };
  const at = body.components?.findIndex((c) => c.inline?.mcp?.transport === "stdio") ?? -1;
  const comp = body.components?.[at];
  if (!comp) return base;
  const id = `inline:${at}`;
  const secret = !!comp.inline?.secrets?.length;
  const row = {
    ...MCP_LOCAL, id, name: comp.name ?? "", hosts: comp.inline?.hosts ?? [], credentials: secret ? [`${id}:secret:0`] : [],
    source: { kind: "component" as const, id, name: comp.name },
    mcp: { ...MCP_LOCAL.mcp!, harnesses: comp.mcp_harnesses ?? ["claude-code", "codex-cli"] },
  };
  return { ...base, rows: [...base.rows, row], credentials: secret ? [{ ...MCP_LOCAL_ENV, id: `${id}:secret:0`, row: id }] : [] };
}

async function stub(page: Page, switchOn: boolean): Promise<string[]> {
  const previews: string[] = [];
  await page.route("**/api/v1/runs/policy-preview", (route) => {
    previews.push(route.request().postData() ?? "");
    return route.fulfill({ json: preview({ access: accessFor(route.request().postData()) }) });
  });
  await page.route("**/api/v1/runs/preflight", (route) => route.fulfill({ json: SCENARIO_PREFLIGHT.member }));
  await page.route("**/api/v1/secrets*", (route) => route.fulfill({ json: { names: ["DOCS_TOKEN"], mine: [] } }));
  await page.route("**/api/v1/me/components", (route) =>
    route.request().method() === "GET" ? route.fulfill({ json: { ...MY_COMPONENTS, may_define: true, resident_delivery_allowed: true } }) : route.fallback(),
  );
  let status: Record<string, unknown> | null = null;
  await page.route("**/api/v1/setup/status*", async (route) => {
    status ??= {
      ...((await (await route.fetch()).json()) as Record<string, unknown>),
      harnesses: SCENARIO_HARNESSES.full,
      harness_sets: SCENARIO_HARNESS_SETS.full,
      features: switchOn ? { mcp_local_servers: true } : {},
    };
    await route.fulfill({ json: status });
  });
  return previews;
}

/** Start, the document path to Review with a complete application draft, then Access's own way back to step 5. */
async function openTools(page: Page) {
  await gotoConsole(page);
  await navToRoute(page, "/runs/new");
  await press(page, NR_START.DOC_YAML);
  await page.getByRole("textbox", { name: NR_DOCUMENT.EDITOR_ARIA("YAML") }).fill(DOCUMENT);
  await press(page, NR_DOCUMENT.CONTINUE_REVIEW);
  await expect(page.getByRole("heading", { name: NR_REVIEW.TITLE })).toBeVisible();
  await press(page, `${NR_REVIEW.CHANGE} ${NR_REVIEW.GROUP_ACCESS}`);
  await press(page, NR_REVIEW.EDIT_TOOLS_ACCESS);
  await expect(page.getByRole("heading", { name: NR_TOOLS.TITLE })).toBeVisible();
}

test.describe("New Run, step 5: an MCP server that runs in the sandbox", () => {
  test("with the switch off the dialog is today's: no tabs, and the local server is Coming soon", async ({ page }) => {
    await stub(page, false);
    await openTools(page);
    const door = page.getByRole("heading", { name: NR_TOOLS.MCP_CARD_TITLE }).locator("xpath=ancestor::li[1]");
    await expect(door).toContainText(NR_TOOLS.MCP_CARD_BODY);
    await press(page, NR_TOOLS.MCP_CARD_BUTTON);
    const dialog = page.getByRole("dialog", { name: NR_TOOLS.MCP_CARD_TITLE });
    await expect(dialog.getByText(NR_TOOLS.MCP_LOCAL_SOON)).toBeVisible();
    await expect(dialog.getByRole("button", { name: NR_TOOLS.MCP_TAB_LOCAL })).toHaveCount(0);
  });

  test("adds a server that runs in the sandbox, says where it runs and who can read its secret, and holds Review for Codex CLI", async ({ page }) => {
    const previews = await stub(page, true);
    await openTools(page);
    const door = page.getByRole("heading", { name: NR_TOOLS.MCP_CARD_TITLE }).locator("xpath=ancestor::li[1]");
    await expect(door).toContainText(NR_TOOLS.MCP_CARD_BODY_LOCAL);

    await press(page, NR_TOOLS.MCP_CARD_BUTTON);
    const dialog = page.getByRole("dialog", { name: NR_TOOLS.MCP_CARD_TITLE });
    await expect(dialog.getByText(NR_TOOLS.MCP_INTRO)).toBeVisible();
    await press(page, NR_TOOLS.MCP_TAB_LOCAL);
    const where = dialog.getByRole("group", { name: NR_TOOLS.DETAIL_WHERE });
    await expect(where.getByRole("radio", { name: NR_TOOLS.MCP_WHERE_RUN })).toBeChecked();
    await expect(where.getByText(NR_TOOLS.MCP_WHERE_RUN_HINT)).toBeVisible();
    await expect(where.getByRole("radio", { name: new RegExp(NR_TOOLS.MCP_WHERE_OWN) })).toBeDisabled();
    await expect(where.getByText(NR_SHARED.COMING_SOON)).toBeVisible();
    await expect(where.getByText(NR_TOOLS.MCP_WHERE_OWN_HINT)).toBeVisible();
    for (const text of [NR_TOOLS.MCP_COMMAND_HELP, NR_TOOLS.NOTE_COMMAND, NR_TOOLS.MCP_HOSTS_HELP, NR_TOOLS.MCP_DELIVERY_NOTE, NR_TOOLS.MCP_HARNESS_RULES]) {
      await expect(dialog.getByText(text)).toBeVisible();
    }

    // An unpinned package is refused on the field, and nothing is added.
    await dialog.getByLabel(NR_TOOLS.MCP_NAME).fill("docs-search");
    await dialog.getByLabel("Command").fill("npx -y @example/mcp-server");
    await dialog.getByRole("button", { name: NR_TOOLS.ADD_TO_RUN }).click();
    await expect(dialog.getByRole("alert")).toHaveText(NR_TOOLS.MCP_ERR_PIN);
    // So are a program that is not npx or an absolute path, and an npx flag: the server refuses both.
    await dialog.getByLabel("Command").fill("uvx foo");
    await dialog.getByRole("button", { name: NR_TOOLS.ADD_TO_RUN }).click();
    await expect(dialog.getByRole("alert")).toHaveText(NR_TOOLS.MCP_ERR_PROGRAM("uvx"));
    await dialog.getByLabel("Command").fill("npx --package x");
    await dialog.getByRole("button", { name: NR_TOOLS.ADD_TO_RUN }).click();
    await expect(dialog.getByRole("alert")).toHaveText(NR_TOOLS.MCP_ERR_NPX_FLAG);
    await dialog.getByLabel("Command").fill(COMMAND);
    await dialog.getByLabel(NR_TOOLS.CC_HOSTS).fill("docs.example.com");
    await dialog.getByLabel(NR_TOOLS.SECRET_LABEL).selectOption("DOCS_TOKEN");
    await dialog.getByRole("radio", { name: NR_TOOLS.DELIVERY_ENV_OPTION }).check();
    await dialog.getByLabel(NR_TOOLS.SECRET_VARIABLE_LABEL).fill("DOCS_TOKEN");
    await dialog.getByRole("button", { name: NR_TOOLS.ADD_TO_RUN }).click();
    await expect(dialog).toHaveCount(0);

    // The body the preview was sent is the server's: stdio and a launch, no url.
    await expect.poll(() => previews.at(-1)).toContain('"transport":"stdio"');
    expect((JSON.parse(previews.at(-1) ?? "{}") as Sent).components?.[0]?.inline?.mcp).toEqual({ transport: "stdio", launch: { command: "npx", args: ["-y", "@example/mcp-server@1.4.2"] } });

    // The added row, the Credentials total and the harness card say what it is.
    const added = page.getByRole("heading", { name: "docs-search", level: 3 }).locator("xpath=ancestor::li[1]");
    await expect(added.getByText(NR_TOOLS.SUB_RUN_ONLY)).toBeVisible();
    const brings = added.getByLabel(NR_TOOLS.BRINGS_FOR("docs-search"));
    await expect(brings.getByText("docs.example.com")).toBeVisible();
    await expect(brings.getByText("DOCS_TOKEN")).toBeVisible();
    await expect(brings.getByText(NR_TOOLS.DELIVERY_ENV)).toBeVisible();
    await expect(brings.getByText(`${NR_TOOLS.FROM_MCP} · ${NR_TOOLS.MCP_RUNS_HERE}`)).toBeVisible();
    await added.getByRole("button", { name: NR_TOOLS.DETAILS_FOR("docs-search") }).click();
    const details = page.getByRole("dialog", { name: "docs-search" });
    await expect(details.getByText(NR_TOOLS.MCP_SANDBOX_READABLE)).toBeVisible();
    await expect(details.getByText(NR_TOOLS.MCP_RUNS_HERE).first()).toBeVisible();
    await expect(details.getByText("npx -y @example/mcp-server@1.4.2")).toBeVisible();
    await expect(details.getByText(NR_TOOLS.SUB_RUN_ONLY)).toBeVisible();
    await expect(details.getByText(NR_TOOLS.MCP_HARNESS_RULES)).toBeVisible();
    await details.getByRole("button", { name: NR_REVIEW.CLOSE, exact: true }).last().click();
    const totals = page.getByRole("region", { name: NR_TOOLS.TOTALS_ARIA });
    await expect(totals.getByText(NR_TOOLS.MCP_SANDBOX_READABLE)).toBeVisible();
    await expect(totals.getByText(NR_TOOLS.DELIVERY_ENV)).toBeVisible();
    const claude = card(page, "Claude Code");
    await expect(claude.getByText(NR_TOOLS.MCP_RUNS_HERE)).toBeVisible();
    await expect(claude.getByText(NR_TOOLS.MCP_HARNESS_RULES)).toBeVisible();

    // Codex CLI cannot apply a rule on this server's tools: the run would be refused.
    const codex = card(page, "Codex CLI");
    await expect(codex.getByRole("alert")).toHaveCount(0);
    await codex.getByRole("button", { name: NR_TOOLS.ADD_RULE, exact: true }).click();
    const rule = page.getByRole("dialog", { name: NR_TOOLS.RULE_TITLE("Codex CLI") });
    await rule.getByLabel(NR_TOOLS.RULE_TOOL).fill("mcp__docs-search__write_page");
    // The radios are sr-only; the label is the target.
    await rule.locator("label", { hasText: NR_TOOLS.EFFECT_DENY }).click();
    await rule.getByRole("button", { name: NR_TOOLS.RULE_CONFIRM }).click();
    const refused = NR_TOOLS.MCP_REFUSED("Codex CLI", "docs-search");
    await expect(codex.getByRole("alert")).toHaveText(refused);

    await press(page, NR_TOOLS.REVIEW_RUN);
    await expect(page.locator("#nr-blockers").getByText(refused)).toBeVisible();
    await expect(page.getByRole("button", { name: NR_REVIEW.LAUNCH_RUN })).toBeDisabled();
    await expect(page.getByText(NR_REVIEW.POLICY_IN_SANDBOX("DOCS_TOKEN", "docs-search"))).toBeVisible();
    await expect(page.getByText(NR_REVIEW.POLICY_HARNESS_RULES("docs-search"))).toBeVisible();
    await press(page, `${NR_REVIEW.CHANGE} ${NR_REVIEW.GROUP_ACCESS}`);
    const access = page.getByRole("group", { name: NR_REVIEW.GROUP_ACCESS });
    await expect(access.getByRole("heading", { name: NR_TOOLS.CREDENTIALS_TITLE })).toBeVisible();
    await expect(access.getByText("DOCS_TOKEN")).toBeVisible();

    // Turning the server off for Codex CLI clears the blocker.
    await page.locator("#nr-blockers").getByRole("button", { name: refused }).click();
    await expect(page.getByRole("heading", { name: NR_TOOLS.TITLE })).toBeVisible();
    await codex.getByRole("checkbox", { name: "docs-search" }).click();
    await expect(codex.getByRole("alert")).toHaveCount(0);
    await press(page, NR_TOOLS.REVIEW_RUN);
    await expect(page.locator("#nr-blockers").getByText(refused)).toHaveCount(0);
  });
});
