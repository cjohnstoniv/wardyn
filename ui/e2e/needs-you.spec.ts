/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #2963 "Needs you" (mock M8 Option A), real backend: a takeover request on a running run shows its
// banner, a row in the Approvals list and a "Needs you" row in Runs; Take over gives the person the
// strip, Hand back is one click to the confirmation, and the audit log holds takeover.begin then
// takeover.close for the request. The request is seeded with SQL: raising it is the sandbox's job
// (wardyn needs-you), pinned by the Go tests.
import { randomUUID } from "node:crypto";
import type { Page } from "@playwright/test";
import { test, expect, ADMIN_TOKEN, sql } from "./fixtures";
import { NEEDS_YOU } from "../src/app/components/wardyn/copy/needs-you";
import { RUNS_ROW_WORD } from "../src/app/components/wardyn/copy/runs-landing";

const auth = { Authorization: `Bearer ${ADMIN_TOKEN}` };
const REASON = "The staging portal sends a code to a phone, so I can't finish signing in.";

async function seedRequest(page: Page, task: string): Promise<{ run: string; ap: string }> {
  const res = await page.request.post("/api/v1/runs", { headers: auth, data: { agent: "claude-code", repo: "acme/widgets", task } });
  expect(res.status(), await res.text()).toBe(201);
  const run = sql(`SELECT id FROM agent_runs WHERE task = '${task}' ORDER BY created_at DESC LIMIT 1`);
  sql(`UPDATE agent_runs SET state = 'RUNNING', interactive = true WHERE id = '${run}'`);
  const ap = randomUUID();
  const scope = JSON.stringify({ reason: REASON });
  sql(
    `INSERT INTO approvals (id, run_id, kind, requested_scope, state, requested_at) VALUES
     ('${ap}','${run}','human_takeover','${scope}'::jsonb,'PENDING',now() - interval '2 minutes')`,
  );
  return { run, ap };
}

test.describe("Needs you (#2963)", () => {
  test("banner, Approvals card and Runs row; take over, hand back, and the audit rows", async ({ page }) => {
    const task = `needs you ${randomUUID().slice(0, 8)}`;
    const { run, ap } = await seedRequest(page, task);
    try {
      // The Approvals list carries it as its own card, and Take over goes to the run.
      await page.goto("approvals");
      const card = page.getByTestId("takeover-card").filter({ hasText: REASON });
      await expect(card.getByText(NEEDS_YOU.card.title)).toBeVisible();
      await expect(card.getByText(NEEDS_YOU.card.blast)).toBeVisible();

      // Runs: the row reads "Needs you to take over".
      await page.goto("runs");
      const row = page.getByTestId("run-row").filter({ hasText: task });
      await expect(row.getByText(RUNS_ROW_WORD.NEEDS_TAKEOVER)).toBeVisible();

      await page.goto(`runs/${run}`);
      const banner = page.getByTestId("needs-you-banner");
      await expect(banner.getByText(NEEDS_YOU.banner.title)).toBeVisible();
      await expect(banner.getByTestId("needs-you-reason")).toHaveText(REASON);
      await banner.getByRole("button", { name: NEEDS_YOU.banner.takeOver }).click();

      const strip = page.getByTestId("needs-you-control");
      await expect(strip.getByText(NEEDS_YOU.strip.control("terminal"))).toBeVisible();
      // Recording off on an unrecorded run; on a recorded one the strip claims nothing (no pause yet).
      await expect(strip).not.toContainText(/paus/i);
      await strip.getByRole("button", { name: NEEDS_YOU.strip.handBack }).click();

      const done = page.getByTestId("needs-you-done");
      await expect(done.getByText(NEEDS_YOU.done.title)).toBeVisible();
      await expect(done.getByText(NEEDS_YOU.done.working)).toBeVisible();
      await expect(done).not.toContainText(/recording/i);
      await expect(page.getByTestId("needs-you-control")).toHaveCount(0);

      // Both ends are audited, in order, for this request.
      const actions = sql(
        `SELECT string_agg(action, ',' ORDER BY seq) FROM audit_events
          WHERE run_id = '${run}' AND action LIKE 'takeover.%' AND data->>'approval_id' = '${ap}'`,
      );
      expect(actions).toBe("takeover.begin,takeover.close");
      expect(sql(`SELECT handed_back_at IS NOT NULL FROM approvals WHERE id = '${ap}'`)).toBe("t");
    } finally {
      sql(`DELETE FROM approvals WHERE id = '${ap}'`);
      sql(`DELETE FROM agent_runs WHERE id = '${run}'`);
    }
  });

  test("Decline answers the request and the banner goes", async ({ page }) => {
    const task = `needs you decline ${randomUUID().slice(0, 8)}`;
    const { run, ap } = await seedRequest(page, task);
    try {
      await page.goto(`runs/${run}`);
      const banner = page.getByTestId("needs-you-banner");
      await banner.getByRole("button", { name: NEEDS_YOU.banner.decline }).click();
      await expect(page.getByTestId("needs-you-banner")).toHaveCount(0);
      expect(sql(`SELECT state FROM approvals WHERE id = '${ap}'`)).toBe("DENIED");
    } finally {
      sql(`DELETE FROM approvals WHERE id = '${ap}'`);
      sql(`DELETE FROM agent_runs WHERE id = '${run}'`);
    }
  });
});
