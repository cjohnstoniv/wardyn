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
  // #1197 L3: this raw UPDATE bypasses the normal state-transition write path
  // (store.go), which is what stamps ended_at on a real terminal transition
  // — without it a COMPLETED row here reads ended_at=NULL, which the Runs
  // landing page's default 7-day ended_within window then silently excludes
  // (store_runs_filtered.go's ageWindowSQL: a NULL end time never satisfies
  // >=). Same fix as scripts/e2e-backend.sh's seed.
  const TERMINAL = new Set(["COMPLETED", "STOPPED", "FAILED", "KILLED", "ARCHIVED"]);
  const endedAt = TERMINAL.has(state) ? ", ended_at = now()" : "";
  sql(`UPDATE agent_runs SET state = '${state}', created_by = '${owner}'${endedAt} WHERE id = '${id}'`);
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

      const mineRow = page.getByTestId("run-row").filter({ hasText: mineTask });
      const theirsRow = page.getByTestId("run-row").filter({ hasText: theirsTask });
      await expect(mineRow.getByText(`${ME} (you)`)).toBeVisible();
      await expect(mineRow.getByRole("button", { name: OPEN_IN_USER_VIEW })).toBeVisible();
      // Not exact: the row's meta line is one dot-joined string (design.md
      // §1's row anatomy), not a separate span per field — same reason
      // mineRow's "(you)" check above isn't exact either.
      await expect(theirsRow.getByText(SOMEONE)).toBeVisible();
      await expect(theirsRow.getByRole("button", { name: OPEN_IN_USER_VIEW })).toHaveCount(0);

      // #1197 D2 removed the kebab menu (Kill/Clone/Open) from the Runs
      // landing page entirely — one inline action at most per row, and the
      // row's title is the ONLY thing that opens it (design.md §5: not
      // role="button", no nested interactive widget). The monitor for that
      // run carries no relaunch either — reached by CLICKING the title link,
      // not page.goto: a goto would reach the monitor even if the row's own
      // link pointed at the wrong view (review finding — every link must
      // stay in the Admin view via ViewGate's TWIN rule, not just the URL
      // typed directly).
      await mineRow.getByRole("link", { name: mineTask }).click();
      await expect(page).toHaveURL(new RegExp(`/admin/runs/${mine}$`));
      await expect(page.getByRole("heading", { name: mineTask, level: 1 })).toBeVisible();
      await expect(page.getByRole("button", { name: RUN.CLONE_CTA })).toHaveCount(0);

      // …and the switch link on the board lands on the owner's own cockpit,
      // where the relaunch is.
      await page.goto("/admin/runs");
      await page.getByTestId("run-row").filter({ hasText: mineTask }).getByRole("button", { name: OPEN_IN_USER_VIEW }).click();
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

  // F2 (#580, PR #1317 review) — the mock's own "Admin" section
  // (long-holds-packet.html:301-303): the "Older limits" chip + bulk
  // "Restart with current limits", over the real routes (RL-10, #575). This
  // harness runs `-runner none` (no real proxy release ever gets stored), so
  // the listing and the restart are both route-mocked, same technique
  // run-lifetime.spec.ts uses for revive.
  test("Older limits chip + Restart with current limits (F2); R2-4: the result survives an empty refetch", async ({ page }) => {
    // R2-4 (PR #1317 round-2 review): a REAL successful restart rewrites the
    // run's own proxy_release (store_run_revive.go), so the very next
    // listing comes back with nothing outside the window — the mock must
    // change after the restart the same way the real server does, or this
    // test would pass against a fiction that can never expose the card
    // hiding its own result.
    let restarted = false;
    await page.route("**/api/v1/admin/runs/proxy-window", (route) =>
      route.fulfill({
        json: {
          release: "0.8.0",
          window: ["0.8", "0.7"],
          outside: restarted ? [] : [{ run_id: "r1", created_by: ME, state: "RUNNING", proxy_release: "0.6" }],
        },
      }),
    );
    let restartedIds: string[] = [];
    await page.route("**/api/v1/admin/runs/restart", async (route) => {
      restartedIds = JSON.parse(route.request().postData() ?? "{}").run_ids;
      restarted = true;
      await route.fulfill({ json: { results: [{ run_id: "r1", ok: true }] } });
    });

    await page.goto("/admin/runs");
    const card = page.getByTestId("admin-older-limits-card");
    await expect(card.getByText("Older limits · 1")).toBeVisible();
    await card.getByRole("button", { name: "Restart with current limits" }).click();
    await expect.poll(() => restartedIds).toEqual(["r1"]);
    // The chip and button are gone (nothing left outside the window), but the
    // restart's own result stays visible — the whole point of R2-4.
    await expect(card.getByText("Older limits · 1")).toHaveCount(0);
    await expect(card.getByRole("button", { name: "Restart with current limits" })).toHaveCount(0);
    await expect(card.getByText(/Restarted/)).toBeVisible();
  });
});
