/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A run with several agents, as the person sees it after launch (F2MULTI-3, F2MULTI-4): the Agents panel and
// the output filter on the Output tab, and "Asked by" on a held tool call (the Approvals card and the run page's
// live strip) and nowhere on a plain egress hold. The seeded runner starts one agent, so the real run's GET is
// spliced to RUNNING with two agents, and the output and approval reads are stubbed; the route and tab wiring
// are the harness's own.
import { randomUUID } from "node:crypto";
import type { Page } from "@playwright/test";
import { test, expect, consoleAPI, gotoConsole, navToRoute } from "./fixtures";
import { RUN_OUTPUT } from "../src/app/components/wardyn/copy";
import { RUN_AGENTS } from "../src/app/components/wardyn/copy/run-output";
import { APPROVAL_ASKED } from "../src/app/components/wardyn/copy/approvals";

const AGENTS = [
  { agent: "claude-code", task: "Fix the flaky login test." },
  { agent: "codex-cli", task: "Write tests for the login retry path." },
];
const OUTPUT = "[claude-code] fixing login.test.ts\n[codex-cli] writing retry.test.ts\n[claude-code] done";

async function multiAgentRun(page: Page): Promise<string> {
  await gotoConsole(page);
  const created = await consoleAPI(page, "POST", "/api/v1/runs", { agent: "claude-code", task: "multi-agent e2e " + randomUUID().slice(0, 8) });
  expect(created.status, created.text).toBe(201);
  const id = JSON.parse(created.text).id as string;
  await page.route(`**/api/v1/runs/${id}`, async (route) => {
    if (route.request().method() !== "GET") return route.fallback();
    const real = (await (await route.fetch()).json()) as Record<string, unknown>;
    await route.fulfill({ json: { ...real, state: "RUNNING", agents: AGENTS } });
  });
  return id;
}

// The PENDING read, for the Approvals screen (all runs) and the live strip (?run_id=).
async function holds(page: Page, rows: Record<string, unknown>[]) {
  await page.route(
    (url) => url.pathname === "/api/v1/approvals",
    (route) => {
      const url = new URL(route.request().url());
      if (route.request().method() !== "GET" || url.searchParams.get("state") !== "PENDING") return route.fallback();
      return route.fulfill({ json: rows });
    },
  );
}

const hold = (runId: string, kind: "tool_call" | "egress_domain", scope: Record<string, unknown>) => ({
  id: randomUUID(),
  run_id: runId,
  kind,
  requested_scope: { harness: "codex-cli", ...scope },
  state: "PENDING",
  requested_at: new Date().toISOString(),
});

test.describe("A run with several agents", () => {
  test("the Output tab lists each agent with its task and status, and the filter shows one agent's lines", async ({ page }) => {
    await page.route("**/api/v1/runs/*/output", (route) =>
      route.fulfill({
        json: {
          output: OUTPUT, truncated: false, complete: false, source: "stdout", incomplete: false, capture_gap: false,
          agents: [{ agent: "claude-code", state: "exited", exit_code: 0 }, { agent: "codex-cli", state: "running" }],
        },
      }),
    );
    const id = await multiAgentRun(page);
    await navToRoute(page, `/runs/${id}`);
    await page.getByRole("tab", { name: RUN_OUTPUT.tab }).click();

    const panel = page.getByRole("region", { name: RUN_AGENTS.title });
    await expect(panel.getByRole("listitem")).toHaveCount(2);
    await expect(panel.getByText(AGENTS[0].task)).toBeVisible();
    await expect(panel.getByText(AGENTS[1].task)).toBeVisible();
    await expect(panel.getByText(RUN_AGENTS.summaryRunning(2, 1, 1))).toBeVisible();
    await expect(panel.getByText(RUN_AGENTS.exitRule)).toBeVisible();

    const pre = page.getByTestId("run-output-text");
    await expect(pre).toContainText("[claude-code] done");
    const filter = page.getByRole("group", { name: RUN_AGENTS.filterLabel });
    await filter.getByRole("button", { name: /codex/i }).click();
    await expect(pre).toContainText("[codex-cli] writing retry.test.ts");
    await expect(pre).not.toContainText("[claude-code]");
    await filter.getByRole("button", { name: RUN_AGENTS.filterAll }).click();
    await expect(pre).toContainText("[claude-code] done");
  });

  test("a held tool call names the agent that asked, on the Approvals card and on the run page's strip", async ({ page }) => {
    const id = await multiAgentRun(page);
    await holds(page, [hold(id, "tool_call", { tool: "Bash", cmd: "npm ci" })]);

    await navToRoute(page, `/runs/${id}`);
    const strip = page.getByTestId("live-approvals");
    await expect(strip.getByTestId("live-approval-row")).toContainText("Bash: npm ci");
    await expect(strip.getByText(/^· Asked by .*Codex/)).toBeVisible();

    await page.getByRole("link", { name: /^Approvals(\s+\d+)?$/ }).click();
    await expect(page.getByRole("heading", { name: "Approvals" })).toBeVisible();
    const asked = page.locator("p", { hasText: new RegExp(`^${APPROVAL_ASKED.LINE_PRE}`) });
    await expect(asked).toContainText(APPROVAL_ASKED.LINE_POST(2));
    await expect(asked).toContainText(/Codex/);
  });

  test("a held egress call names no agent, even on a run with several", async ({ page }) => {
    const id = await multiAgentRun(page);
    const host = `egress-${randomUUID().slice(0, 8)}.example.test`;
    await holds(page, [hold(id, "egress_domain", { host })]);

    await navToRoute(page, `/runs/${id}`);
    const row = page.getByTestId("live-approvals").getByTestId("live-approval-row");
    await expect(row).toContainText(host);
    await expect(page.getByText(/Asked by/)).toHaveCount(0);

    await page.getByRole("link", { name: /^Approvals(\s+\d+)?$/ }).click();
    await expect(page.getByRole("heading", { name: "Approvals" })).toBeVisible();
    await expect(page.getByText(host).first()).toBeVisible();
    await expect(page.getByText(/Asked by/)).toHaveCount(0);
  });
});
