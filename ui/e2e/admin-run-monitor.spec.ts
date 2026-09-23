/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { randomUUID } from "node:crypto";
import type { Page } from "@playwright/test";
import { test, expect, ADMIN_TOKEN, sql } from "./fixtures";
import { RUN } from "../src/app/components/wardyn/copy";
import { REAUTH_ROW } from "../src/app/components/wardyn/model-access-copy";
import { OPEN_IN_USER_VIEW } from "../src/app/components/wardyn/copy/console-view";

// M-7 — the admin run monitor (admin-member-modes-design.md §4.6, §6; modes-b
// §1: "Monitor, kill, decide. No New run and no relaunch. The admin's own run
// carries only a switch link.").
//
// The seeded backend is a single-operator install (a bare admin bearer, no
// identity provider): both views, the URL decides, so "Open in user view" only
// navigates. The SSO path (POST /me/member-mode, then reload) is pinned in
// open-in-user-view.test.tsx. The viewer's subject is spliced onto /me so this
// spec's own runs are "the admin's own", the idiom runs.spec.ts's
// credential-door case uses. Each test makes its own runs, so none of the
// shared fixtures change hands.

const auth = { Authorization: `Bearer ${ADMIN_TOKEN}` };
const ME = "monitor-admin@e2e.example";
const SOMEONE = "monitor-user@e2e.example";

async function asMe(page: Page): Promise<void> {
  await page.route("**/api/v1/me", async (route) => {
    const response = await route.fetch();
    const json = await response.json();
    json.principal = ME;
    await route.fulfill({ response, json });
  });
}

async function createRun(page: Page, task: string, owner: string, state: string): Promise<string> {
  const res = await page.request.post("/api/v1/runs", {
    headers: auth,
    data: { agent: "claude-code", repo: "acme/widgets", task },
  });
  expect(res.status(), await res.text()).toBe(201);
  const id = sql(`SELECT id FROM agent_runs WHERE task = '${task}' ORDER BY created_at DESC LIMIT 1`);
  sql(`UPDATE agent_runs SET state = '${state}', created_by = '${owner}' WHERE id = '${id}'`);
  return id;
}

test.describe("the admin run monitor (M-7)", () => {
  test("/admin/runs names every owner, marks the admin's own, and offers no New run or relaunch", async ({ page }) => {
    const suffix = randomUUID().slice(0, 8);
    const mineTask = `monitor mine ${suffix}`;
    const theirsTask = `monitor theirs ${suffix}`;
    const mine = await createRun(page, mineTask, ME, "COMPLETED");
    const theirs = await createRun(page, theirsTask, SOMEONE, "COMPLETED");
    try {
      await asMe(page);
      await page.goto("/admin/runs");
      await expect(page.getByRole("heading", { name: "Runs", level: 1 })).toBeVisible();
      await expect(page.getByText(/Every run, live/)).toBeVisible();
      await expect(page.getByRole("button", { name: "New run" })).toHaveCount(0);

      const mineCard = page.getByTestId("run-card").filter({ hasText: mineTask });
      const theirsCard = page.getByTestId("run-card").filter({ hasText: theirsTask });
      await expect(mineCard.getByText(`${ME} (you)`)).toBeVisible();
      await expect(mineCard.getByRole("button", { name: OPEN_IN_USER_VIEW })).toBeVisible();
      await expect(theirsCard.getByText(SOMEONE, { exact: true })).toBeVisible();
      await expect(theirsCard.getByRole("button", { name: OPEN_IN_USER_VIEW })).toHaveCount(0);

      // No relaunch in the kebab of a finished run, even the admin's own.
      await mineCard.getByRole("button", { name: "Run actions" }).click();
      await expect(page.getByRole("menuitem", { name: "Open detail" })).toBeVisible();
      await expect(page.getByRole("menuitem", { name: RUN.CLONE_CTA })).toHaveCount(0);
      await page.keyboard.press("Escape");

      // The monitor for that run carries no relaunch either…
      await page.goto(`/admin/runs/${mine}`);
      await expect(page.getByRole("heading", { name: mineTask, level: 1 })).toBeVisible();
      await expect(page.getByRole("button", { name: RUN.CLONE_CTA })).toHaveCount(0);

      // …and the switch link on the board lands on the owner's own cockpit,
      // where the relaunch is.
      await page.goto("/admin/runs");
      await mineCard.getByRole("button", { name: OPEN_IN_USER_VIEW }).click();
      await expect(page).toHaveURL(new RegExp(`/runs/${mine}$`));
      await expect(page.getByRole("button", { name: RUN.CLONE_CTA })).toBeVisible();
    } finally {
      sql(`DELETE FROM agent_runs WHERE id IN ('${mine}','${theirs}')`);
    }
  });

  test("a held per-user sign-in on the admin's own run: no door in the monitor, the switch link opens the owner's door", async ({
    page,
  }) => {
    const task = `monitor held ${randomUUID().slice(0, 8)}`;
    const id = await createRun(page, task, ME, "RUNNING");
    const scope = JSON.stringify({ mechanism: "bedrock_sso", credential_source: "per_user", owner: ME });
    sql(
      `INSERT INTO approvals (id, run_id, kind, requested_scope, state, requested_at) VALUES
       ('${randomUUID()}','${id}','credential_reauth','${scope}'::jsonb,'PENDING',now())`,
    );
    try {
      await asMe(page);
      await page.goto(`/admin/runs/${id}`);
      const panel = page.getByTestId("live-approvals");
      await expect(panel.getByText(REAUTH_ROW.notYoursHint(ME))).toBeVisible();
      await expect(panel.getByRole("button", { name: REAUTH_ROW.ariaLabel })).toHaveCount(0);

      await panel.getByRole("button", { name: OPEN_IN_USER_VIEW }).click();
      await expect(page).toHaveURL(new RegExp(`/runs/${id}$`));
      await expect(page.getByTestId("live-approvals").getByRole("button", { name: REAUTH_ROW.ariaLabel })).toBeVisible();
    } finally {
      sql(`DELETE FROM agent_runs WHERE id = '${id}'`);
    }
  });
});
