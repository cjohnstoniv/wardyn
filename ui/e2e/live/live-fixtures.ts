/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test as base, expect, type Locator, type Page } from "@playwright/test";
import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";

// Fixtures for the LIVE Playwright project (playwright.config.ts project "live").
// These specs drive the REAL host-mode stack started by scripts/run-host.sh —
// real wardynd on :8080, the docker runner (real sandboxes), the real composer —
// and launch REAL runs. They are opt-in: without WARDYN_E2E_LIVE=1 every spec
// self-skips (see liveOnly), so an accidental `--project=live` no-ops.
//
// Run: WARDYN_E2E_LIVE=1 pnpm e2e --project=live   (ideally --workers=1)

export const LIVE = !!process.env.WARDYN_E2E_LIVE;

// Host mode is local-mode (no bearer required same-origin), but the app still
// reads/stores a token — inject one so specs boot straight into the console.
export const ADMIN_TOKEN = process.env.WARDYN_E2E_ADMIN_TOKEN || "demo-admin-token";
const TOKEN_KEY = "wardyn_admin_token";

export const AUTH_HEADERS = { Authorization: `Bearer ${ADMIN_TOKEN}` };

// Call at the top of every live spec file: skips the whole file unless the
// operator explicitly opted into driving a live stack. (Body runs at call time,
// after module evaluation, so referencing `test` below is safe.)
export function liveOnly(): void {
  test.skip(!LIVE, "live stack required — WARDYN_E2E_LIVE=1 against a running scripts/run-host.sh");
}

// Pre-authenticated page, same pattern as ../fixtures.ts.
export const test = base.extend({
  page: async ({ page }, use) => {
    await page.addInitScript(
      ([key, tok]) => {
        try {
          localStorage.setItem(key, tok);
          // Mark the onboarding welcome hero seen so "Getting started" lands
          // directly on the setup funnel (the hero otherwise gates it until a
          // human clicks "Get set up").
          localStorage.setItem("wardyn-onboarding-seen", "1");
        } catch {
          /* private mode — ignore */
        }
      },
      [TOKEN_KEY, ADMIN_TOKEN]
    );
    await use(page);
  },
});

export { expect };

// gotoConsole loads the app shell (pre-authed) and waits for the sidebar.
export async function gotoConsole(page: Page): Promise<void> {
  await page.goto("/");
  await expect(page.getByRole("link", { name: /^Runs/ })).toBeVisible({ timeout: 30_000 });
}

// Open the shell top-bar "New run" dialog and wait for the composer-backends
// probe to resolve BEFORE interacting (its completion re-sets the chooser).
export async function openNewRunChooser(page: Page): Promise<Locator> {
  await gotoConsole(page);
  const backends = page.waitForResponse((r) => /\/composer\/backends/.test(r.url()));
  await page.getByRole("button", { name: "New run" }).click();
  await backends;
  const dlg = page.getByRole("dialog");
  await expect(dlg.getByRole("heading", { name: "New run" })).toBeVisible();
  return dlg;
}

// Enter the manual 5-step PermissionWizard on its Basics step.
export async function openManualWizard(page: Page): Promise<Locator> {
  const dlg = await openNewRunChooser(page);
  await dlg.getByRole("button", { name: /Configure manually/ }).click();
  await expect(dlg.getByText("Workspaces", { exact: true })).toBeVisible();
  return dlg;
}

// Make a real, empty workspace dir on this host (the live stack's docker daemon
// mounts host paths, and the Playwright process runs on the same box).
export function makeWorkspaceDir(prefix: string): string {
  return fs.mkdtempSync(path.join(os.tmpdir(), `${prefix}-`));
}

// Host ~/.claude dir for subscription-mode runs. Override with
// WARDYN_E2E_CLAUDE_DIR (e.g. an operator-staged COPY from stage-claude-creds).
export function claudeDir(): string {
  return process.env.WARDYN_E2E_CLAUDE_DIR || path.join(os.homedir(), ".claude");
}

// Fire `trigger` (which must cause POST /api/v1/runs) and return the created
// run's id from the 201 response.
export async function captureRunCreate(page: Page, trigger: () => Promise<void>): Promise<string> {
  const respPromise = page.waitForResponse(
    (r) => r.request().method() === "POST" && /\/api\/v1\/runs$/.test(r.url()),
  );
  await trigger();
  const resp = await respPromise;
  if (resp.status() !== 201) {
    throw new Error(`run create failed (HTTP ${resp.status()}): ${await resp.text().catch(() => "")}`);
  }
  const body = (await resp.json()) as { id?: string };
  expect(body.id, "create response carried no run id").toBeTruthy();
  return body.id!;
}

// Best-effort API kill — cleanup for interactive runs that would otherwise idle
// forever awaiting attach.
export async function apiKillRun(page: Page, runId: string): Promise<void> {
  await page.request
    .post(`/api/v1/runs/${encodeURIComponent(runId)}/kill`, { headers: AUTH_HEADERS })
    .catch(() => {});
}

// RunStatusBadge labels for terminal states (run-status-badge.tsx). ARCHIVED is
// unreachable for a freshly-launched run.
const TERMINAL_BADGES = ["Completed", "Failed", "Stopped", "Killed"] as const;

// Waits on the CURRENT run-detail page (which live-polls the run) until the
// status badge shows a terminal state, then fails LOUDLY unless it is Completed.
export async function waitForRunTerminal(
  page: Page,
  { timeout = 240_000 }: { timeout?: number } = {},
): Promise<void> {
  let seen: string | null = null;
  await expect
    .poll(
      async () => {
        for (const label of TERMINAL_BADGES) {
          const visible = await page
            .getByText(label, { exact: true })
            .first()
            .isVisible()
            .catch(() => false);
          if (visible) {
            seen = label;
            return label;
          }
        }
        return null;
      },
      {
        timeout,
        intervals: [2_000],
        message: "waiting for the run to reach a terminal state (PENDING→STARTING→RUNNING→COMPLETED)",
      },
    )
    .not.toBeNull();
  expect(seen, `run reached terminal state "${seen}" — expected Completed`).toBe("Completed");
}
