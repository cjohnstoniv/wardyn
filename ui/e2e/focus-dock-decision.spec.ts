/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, gotoConsole, sql } from "./fixtures";
import { randomUUID } from "node:crypto";
import type { Page } from "@playwright/test";
import { RUN_COCKPIT } from "../src/app/components/wardyn/copy";

// #2912: in focus mode the Egress dock panel covered Approve and Deny. The approvals that hold the run now sit in
// their own row under the session, which no panel reaches, at desktop and phone widths.
test.use({ colorScheme: "dark" });
// The second test finds the request the first one seeded (one backend per spec file).
test.describe.configure({ mode: "serial" });

// Under the strip's 72-character clip, but too long to sit beside "waiting", Approve and Deny at 390px.
const CMD = "bash.exec --run the-nightly-integration-suite-with-extra-long-name";
const TITLE = `Run ${CMD}`;

async function holdRun(): Promise<string> {
  const id = sql("SELECT id FROM agent_runs ORDER BY created_at LIMIT 1");
  sql(`UPDATE agent_runs SET state = 'RUNNING' WHERE id = '${id}'`);
  if (sql(`SELECT count(*) FROM approvals WHERE run_id = '${id}' AND state = 'PENDING'`) === "0") {
    sql(`INSERT INTO approvals (id, run_id, kind, requested_scope, state, requested_at)
         VALUES ('${randomUUID()}', '${id}', 'tool_call', '${JSON.stringify({ tool: "bash.exec", cmd: CMD })}'::jsonb, 'PENDING', now() - interval '5 min')`);
  }
  return id;
}

async function expectUncovered(page: Page, name: "Approve" | "Deny") {
  const button = page.getByRole("button", { name, exact: true });
  await expect(button).toBeVisible();
  const [x, y] = await button.evaluate((el) => {
    const r = el.getBoundingClientRect();
    return [r.x + r.width / 2, r.y + r.height / 2];
  });
  const top = await page.evaluate(([px, py]) => {
    const hit = document.elementFromPoint(px, py);
    return hit?.closest("button")?.textContent?.trim() ?? hit?.tagName ?? null;
  }, [x, y]);
  expect(top).toContain(name);
}

test("with the Egress panel open at 1440, Approve and Deny are visible and not covered", async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 });
  const id = await holdRun();
  await gotoConsole(page);
  await page.goto(`/runs/${id}`);
  await page.getByRole("button", { name: RUN_COCKPIT.enterFocus }).click();
  await expect(page.getByRole("button", { name: RUN_COCKPIT.exitFocus })).toBeVisible();
  await expect(page.getByRole("region", { name: "Egress" })).toBeVisible();
  await expect(page.getByText("Sandbox is waiting — approve to let it through")).toBeVisible();
  await expectUncovered(page, "Approve");
  await expectUncovered(page, "Deny");
});

test("at 390 the dock starts closed, the title is whole, and 1 held is on one line", async ({ page }) => {
  const id = await holdRun();
  await gotoConsole(page); // the sidebar is a drawer at phone width, so land first and narrow after
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto(`/runs/${id}`);
  await page.getByRole("button", { name: RUN_COCKPIT.enterFocus }).click();
  await expect(page.getByRole("button", { name: RUN_COCKPIT.exitFocus })).toBeVisible();
  await expect(page.getByRole("region", { name: "Egress" })).toHaveCount(0);
  await expectUncovered(page, "Approve");
  await expectUncovered(page, "Deny");
  const title = page.getByTestId("live-approval-row").getByText(TITLE, { exact: true });
  await expect(title).toBeVisible();
  // Whole, not clipped by an ellipsis, and the verdict controls sit on the line under it.
  expect(await title.evaluate((el) => el.scrollWidth <= el.clientWidth)).toBe(true);
  const [t, a] = [(await title.boundingBox())!, (await page.getByRole("button", { name: "Approve", exact: true }).boundingBox())!];
  expect(a.y).toBeGreaterThanOrEqual(t.y + t.height - 2);
  // An open panel still leaves the bar reachable.
  await page.getByRole("button", { name: RUN_COCKPIT.showWidget("Egress") }).click();
  const panel = page.getByRole("region", { name: "Egress" });
  await expect(panel).toBeVisible();
  // The panel spans from the session's left edge to the rail, not a 55vw sliver.
  expect((await panel.boundingBox())!.x).toBeLessThan(24);
  await expectUncovered(page, "Approve");
  await expectUncovered(page, "Deny");
  const strip = page.locator("div.border-t.backdrop-blur").filter({ hasText: RUN_COCKPIT.held(1) });
  const held = strip.getByText(RUN_COCKPIT.held(1), { exact: true });
  await expect(held).toBeVisible();
  expect((await held.boundingBox())!.height).toBeLessThan(24);
});
