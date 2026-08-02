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

import { randomUUID } from "node:crypto";
import { fileURLToPath } from "node:url";
import path from "node:path";
import type { Page } from "@playwright/test";
import { test, expect, gotoConsole, sql, ADMIN_TOKEN } from "../fixtures";

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

  // 1 RUNNING / 1 WAITING_FOR_CONFIRMATION / 1 FAILED / 2 COMPLETED / 1 PENDING,
  // keyed by created order (the e2e-backend.sh UPDATE-by-created-order
  // technique). "Needs your attention" is a two-column grid, so it needs two
  // cards to not photograph half-empty — and the second is FAILED, a state the
  // product actually reaches (WAITING_FOR_CONFIRMATION is reserved, see
  // types.go RunWaiting). Both are in ATTENTION_STATES (runs.tsx).
  sql(
    `WITH ordered AS (
       SELECT id, row_number() OVER (ORDER BY created_at) AS rn
       FROM agent_runs WHERE task NOT LIKE 'e2e fixture%'
     )
     UPDATE agent_runs a SET state = v.state
     FROM ordered o
     JOIN (VALUES
       (1,'RUNNING'),(2,'WAITING_FOR_CONFIRMATION'),(3,'FAILED'),
       (4,'COMPLETED'),(5,'COMPLETED'),(6,'PENDING')
     ) AS v(rn,state) ON v.rn = o.rn
     WHERE a.id = o.id`,
  );

  // One PENDING approval bound to the first WAITING run so the board's attention
  // badge and the Approvals count read true (INSERT shape from approvals.spec.ts).
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
        // /setup renders the WELCOME hero ("Run anything. Keep your keys.") until
        // this flag is set — the funnel only replaces it once the operator clicks
        // "Get started" (onboarding-screen.tsx GettingStarted). Pre-seeding it
        // lands the capture straight on the funnel.
        localStorage.setItem("wardyn-onboarding-seen", "1");
      } catch {
        /* private mode — ignore */
      }
    });
  });

  // getting-started.png (the README hero): the barrier picker on step 1. This
  // none-runner backend would photograph as a red "No sandbox runner" card, so
  // the ONE call the picker reads — GET /setup/status (environment-step.tsx
  // branches on runner.confinement_classes + platform.kvm) — is served from a
  // fixture: Fence Ready, Wall + Vault honestly "Needs setup", exactly what a
  // real single-host install shows. kvm:true keeps Vault at "Needs setup"
  // rather than "Incompatible here" (that split is a hardware fact).
  test("getting-started.png — barrier picker", async ({ page }) => {
    await page.route("**/api/v1/setup/status", (route) =>
      route.fulfill({
        json: {
          ready: true,
          checks: [],
          auth: { mode: "local", local_loopback: true },
          runner: {
            driver: "docker",
            confinement_classes: ["CC1"],
            confinement_substrates: { CC1: "runc" },
          },
          composer: { enabled: false, backends: [] },
          providers: [],
          secrets: { present: [], github_app: false },
          age_key: { durable: true },
          // has_runs:true only to clear the hard first-run gate (setup-gate.ts
          // setupGateActive), which hides every nav group — with it false the
          // README hero photographs as a console with an empty black left rail.
          // The funnel itself is unchanged; the "First run launched" line it
          // enables sits on the Launch step, far below this shot's fold.
          has_runs: true,
          platform: { os: "linux", wsl: false, kvm: true },
        },
      }),
    );
    // Straight to /setup rather than gotoConsole: this shot is the funnel, and
    // the fixture only routes /setup/status.
    await page.goto("/setup");
    await expect(page.getByRole("heading", { name: "Getting started", level: 1 })).toBeVisible();
    const matrix = page.getByRole("radiogroup", { name: "Barrier tier" });
    await expect(matrix.getByText("Ready", { exact: true })).toHaveCount(1);
    await expect(matrix.getByText("Needs setup", { exact: true })).toHaveCount(2);
    // The host-status strip and sidebar chip both poll — don't catch "Checking…".
    await expect(page.getByText(/Checking/)).toHaveCount(0, { timeout: 10_000 });
    await settle(page);
    await page.screenshot({ path: path.join(DOCS_IMG, "getting-started.png") });
  });

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

});
