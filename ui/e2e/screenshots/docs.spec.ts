/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * Docs screenshot capture — regenerates the UI PNGs under docs/img that the
 * README / TRY-IT reference. This is NOT part of the hermetic PR gate: it lives
 * in its own Playwright `screenshots` project (the chromium gate ignores
 * screenshots/**), viewport pinned to 1440×900, dark theme forced. Driven by
 * scripts/screenshots.sh (`make screenshots`) against a dedicated backend
 * (:8098 / DB wardyn_shots). Run after a visible UI change, then commit the diff.
 *
 * There is deliberately NO pixel gate (screenshot diffs flake). MANUAL REVIEW
 * CHECKLIST — eyeball each regenerated PNG before committing:
 *   - dark theme (Wardyn's dark-first console), not light
 *   - 1440×900 viewport, no horizontal/vertical scrollbars in frame
 *   - no test-fixture strings visible: "e2e fixture N", "wardyn-e2e", seeded
 *     admin tokens, localhost ports
 *   - no stray focus rings, hover highlights, or toasts caught mid-capture
 *   - the intended state is fully shown (cards rendered, dialog fully open)
 *
 * A /demos catalog capture is deliberately absent: on this none-runner backend
 * the demo cards render in their disabled "needs the sandbox runner" state,
 * which would photograph misleadingly for docs. Add a demos.png block if a
 * runner-backed capture backend ever exists.
 */

import { execFileSync } from "node:child_process";
import { randomUUID } from "node:crypto";
import { fileURLToPath } from "node:url";
import path from "node:path";
import type { Page } from "@playwright/test";
import { test, expect, gotoConsole, navTo, ADMIN_TOKEN } from "../fixtures";

// Self-skip unless driven by scripts/screenshots.sh: a bare `pnpm e2e` runs ALL
// projects, and without this guard it would overwrite the tracked docs/img PNGs
// from the wrong backend (:8088 e2e seed) and fail on the missing wardyn_shots DB.
test.skip(
  !process.env.WARDYN_SCREENSHOTS,
  "docs screenshot capture — run via `make screenshots` (exports WARDYN_SCREENSHOTS=1)",
);

// docs/img resolved from this file (ui/e2e/screenshots/) so the output path is
// independent of the process cwd: ../../../docs/img == <repo>/docs/img.
const DOCS_IMG = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../../../docs/img");

// Same daemon/db knobs as the sibling SQL-seeding spec (approvals.spec.ts): we
// talk to the backend's Postgres via `docker exec`. Defaults match the values
// scripts/screenshots.sh exports (its own DB so it never clobbers the e2e DB).
const PG_CONTAINER = process.env.WARDYN_E2E_PG_CONTAINER || "wardyn-test-pg";
const PG_DBNAME = process.env.WARDYN_E2E_PG_DBNAME || "wardyn_shots";

// Run one SQL statement against the backend's Postgres (-tA: tuple-only,
// unaligned — the caller parses a single scalar trivially).
function sql(statement: string): string {
  return execFileSync(
    "docker",
    ["exec", "-i", PG_CONTAINER, "psql", "-U", "wardyn", "-d", PG_DBNAME, "-tAc", statement],
    { encoding: "utf8" },
  ).trim();
}

// Presentable runs for the board shot — realistic tasks, a spread of agents and
// repos. Order matters: the state UPDATE below keys off created (POST) order.
const PRESENTABLE_RUNS: { agent: string; repo: string; task: string }[] = [
  { agent: "claude-code", repo: "acme/payments", task: "Fix flaky webhook retry test" },
  { agent: "codex-cli", repo: "acme/widgets", task: "Migrate CI to pinned Go toolchain" },
  { agent: "claude-code", repo: "acme/api", task: "Add rate-limit headers to the public API" },
  { agent: "claude-code", repo: "acme/payments", task: "Upgrade Postgres driver and fix deprecations" },
  { agent: "codex-cli", repo: "acme/docs", task: "Write release notes for v0.2" },
  { agent: "claude-code", repo: "acme/platform", task: "Refactor session cache eviction" },
];

// Swap the stock `e2e fixture N` seed for the presentable set, then diversify
// states so the board shows a realistic mix. Mirrors the psql+API pattern in
// approvals.spec.ts / e2e-backend.sh (POST via the API, re-state via SQL).
async function restagePresentableRuns(page: Page): Promise<void> {
  // Drop the stock seed (credential_grants + approvals cascade via FK; the seed
  // creates none for these, so nothing else goes with them).
  sql("DELETE FROM agent_runs WHERE task LIKE 'e2e fixture%'");

  // POST the realistic runs (they land PENDING under the `none` runner).
  for (const r of PRESENTABLE_RUNS) {
    const res = await page.request.post("/api/v1/runs", {
      headers: { Authorization: `Bearer ${ADMIN_TOKEN}` },
      data: { agent: r.agent, repo: r.repo, task: r.task },
    });
    expect(res.ok(), `POST run "${r.task}" failed (${res.status()})`).toBeTruthy();
  }

  // 2 RUNNING / 1 WAITING_FOR_CONFIRMATION / 2 COMPLETED / 1 PENDING, keyed by
  // created order (the e2e-backend.sh UPDATE-by-created-order technique).
  sql(
    `WITH ordered AS (
       SELECT id, row_number() OVER (ORDER BY created_at) AS rn
       FROM agent_runs WHERE task NOT LIKE 'e2e fixture%'
     )
     UPDATE agent_runs a SET state = v.state
     FROM ordered o
     JOIN (VALUES
       (1,'RUNNING'),(2,'RUNNING'),(3,'WAITING_FOR_CONFIRMATION'),
       (4,'COMPLETED'),(5,'COMPLETED'),(6,'PENDING')
     ) AS v(rn,state) ON v.rn = o.rn
     WHERE a.id = o.id`,
  );

  // One PENDING approval bound to the WAITING run so the board's attention badge
  // and the Approvals count read true (INSERT shape from approvals.spec.ts).
  const waitingId = sql(
    "SELECT id FROM agent_runs WHERE state = 'WAITING_FOR_CONFIRMATION' ORDER BY created_at LIMIT 1",
  );
  if (waitingId) {
    sql(
      `INSERT INTO approvals (id, run_id, kind, requested_scope, state, requested_at)
       VALUES ('${randomUUID()}', '${waitingId}', 'egress_domain', '{"domain":"api.stripe.com","port":443}'::jsonb, 'PENDING', now())`,
    );
  }
}

// Drop focus rings + hover highlights and let transitions finish before the shot.
async function settle(page: Page): Promise<void> {
  await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur());
  await page.mouse.move(0, 0);
  await page.waitForTimeout(400);
}

test.describe("Docs screenshots", () => {
  test.beforeEach(async ({ page }) => {
    // Force the dark-first console deterministically (theme-provider.tsx storage
    // key), before first navigation — same addInitScript pattern as the admin
    // token in fixtures.ts.
    await page.addInitScript(() => {
      try {
        localStorage.setItem("wardyn-theme", "dark");
      } catch {
        /* private mode — ignore */
      }
    });
  });

  // getting-started.png is NOT captured here: on this none-runner backend the
  // barrier picker renders a red "No sandbox runner — runs can't launch" fatal
  // banner with zero tiers ready, which photographs as a broken product. That
  // hero is captured MANUALLY from a live host-mode instance (make setup, real
  // docker runner → Fence genuinely Ready, Wall/Vault honestly "Needs setup"),
  // /setup → "Finish setup", 1440×900 dark — the documented exception to the
  // scripted set. Re-do it by hand after a visible funnel redesign.

  // runs-board.png (new): the Runs board after re-staging presentable data.
  test("runs-board.png — populated runs board", async ({ page }) => {
    await restagePresentableRuns(page);
    await gotoConsole(page);
    await expect(page.getByRole("heading", { name: "Runs", level: 1 })).toBeVisible();
    // Wait for the re-staged cards to render (first + last of the set).
    await expect(page.getByText("Fix flaky webhook retry test")).toBeVisible();
    await expect(page.getByText("Refactor session cache eviction")).toBeVisible();
    // The sidebar Getting-started chip polls setup status — don't capture its
    // transient "Checking..." state.
    await expect(page.getByText(/Checking/)).toHaveCount(0, { timeout: 10_000 });
    await settle(page);
    await page.screenshot({ path: path.join(DOCS_IMG, "runs-board.png") });
  });

  // tier-matrix.png (new): the New Run wizard → Confinement → "Compare all
  // three" barrier matrix dialog. Navigation is self-contained (no imports from
  // wizard.spec.ts — a parallel agent is editing it + the wizard's workspace
  // validation); we only replicate the few selectors we need.
  test("tier-matrix.png — barrier compare matrix", async ({ page }) => {
    await gotoConsole(page);
    const dlg = page.getByRole("dialog");

    // Open "New run". With composer backends seeded (e2e-backend.sh), the chooser
    // appears first → reach the manual wizard via "Configure manually".
    const backends = page.waitForResponse((r) => /\/composer\/backends/.test(r.url()));
    await page.getByRole("button", { name: "New run" }).click();
    await backends;
    await expect(dlg.getByRole("heading", { name: "New run" })).toBeVisible();
    const manual = dlg.getByRole("button", { name: /Configure manually/ });
    if (await manual.isVisible().catch(() => false)) await manual.click();
    await expect(dlg.getByText("Workspaces", { exact: true })).toBeVisible();

    // Attach the seeded `payments` workspace (optional after the parallel wizard
    // change, but gives nicer data + keeps Next un-gated either way). The option
    // list renders in a portal OUTSIDE the wizard dialog.
    await dlg.getByRole("combobox", { name: /Add a workspace/ }).click();
    await page.getByRole("option", { name: /payments/ }).click();

    // Basics → Access → Egress → Confinement (defaults are all valid).
    for (let i = 0; i < 3; i++) await dlg.getByRole("button", { name: "Next" }).click();
    await expect(dlg.getByText("Barrier").first()).toBeVisible();

    // Open the tier matrix (step-confinement.tsx "Compare all three →").
    await dlg.getByRole("button", { name: /Compare all three/ }).click();
    const matrix = page.getByRole("dialog", { name: /Compare the three barriers/ });
    await expect(matrix.getByText(/Isolated from your files/).first()).toBeVisible();
    await settle(page);
    await page.screenshot({ path: path.join(DOCS_IMG, "tier-matrix.png") });
  });
});
