/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, sql } from "./fixtures";
import type { Page } from "@playwright/test";

// ============================================================================
// The UI-apps lane on run detail (docs/UI-SANDBOXES.md, D3.1) against the
// SEEDED backend — not a stubbed one. scripts/e2e-backend.sh boots wardynd with
// the UI-sandbox gateway's second listener on and seeds one RUNNING run whose
// INLINE policy declares a "vscode" app, so /healthz's ui_sandbox block and
// GET /runs/{id}'s ui_apps denormalization are both real here.
//
// What this file is for: the AFFORDANCE. The relay itself is proven live by
// scripts/run-e2e-ui-sandbox.sh (a real ticket, a real cookie, code-server's
// own HTML through the exec lane); the `none` runner behind this backend has no
// sandbox to relay into. So the assertions here are what the console shows and
// what it DOES on click — including that Open leaves for a different origin and
// never embeds the app in this page.
//
// The strings are the frozen table in docs/design/ui-sandboxes-prompt.md §7,
// the same bytes run-detail-ssh.test.tsx pins at the unit level.
// ============================================================================

const OFF_LINE = /^Off on this deployment\. It relays a declared loopback port inside the sandbox/;
const NO_APPS_LINE = /^On for this deployment, but this run's policy declares no UI apps\./;
const NEW_TAB_LINE =
  "Opens in a new tab, on a different address than this console. That separation is deliberate: the app is the sandbox's own code, and it must never be able to read your console session.";
const NO_RECORDING_LINE =
  "Session recording does not capture this: no keystrokes, no screen, no page content. Wardyn records that you opened and closed the app, never what you did in it.";

// The one seeded RUNNING run — the lane is owner-and-RUNNING-only, and this is
// the fixture the seeder attaches the ui_apps envelope to.
const RUN_TASK = "e2e fixture 2";

function runIdByTask(task: string): string {
  const id = sql(`SELECT id FROM agent_runs WHERE task = '${task}' LIMIT 1`);
  expect(id, `no seeded run with task ${task}`).toMatch(/^[0-9a-f-]{36}$/);
  return id;
}

// window.open is stubbed BEFORE the app loads: the assertion is what the
// console hands the browser, and letting a real popup open would navigate to a
// gateway whose run has no sandbox behind it (the `none` runner) — a 409 that
// says nothing about the affordance.
async function stubWindowOpen(page: Page): Promise<void> {
  await page.addInitScript(() => {
    (window as unknown as { __opened: string[] }).__opened = [];
    window.open = ((url?: string | URL) => {
      (window as unknown as { __opened: string[] }).__opened.push(String(url ?? ""));
      return null;
    }) as typeof window.open;
  });
}

async function openRunDetail(page: Page, task: string): Promise<void> {
  await page.goto(`/runs/${runIdByTask(task)}`);
  await expect(page.getByRole("heading", { name: task, level: 1 })).toBeVisible();
  // The card is the last widget in the live rail, so it may be below its
  // container's fold.
  await page.getByText("UI apps", { exact: true }).scrollIntoViewIfNeeded();
}

test.describe("Run detail — UI apps lane", () => {
  test("a declared app renders its row, its address and both honesty notices", async ({ page }) => {
    await openRunDetail(page, RUN_TASK);

    await expect(page.getByRole("button", { name: "Open vscode" })).toBeVisible();
    await expect(page.getByText("localhost:8080/")).toBeVisible();
    await expect(page.getByText(NEW_TAB_LINE)).toBeVisible();
    await expect(page.getByText(NO_RECORDING_LINE)).toBeVisible();
    // Never embedded: an iframe on the console origin is the exact attack the
    // gateway's second listener exists to prevent.
    expect(await page.locator("iframe").count()).toBe(0);
  });

  test("Open mints a ticket and leaves for the gateway's own origin", async ({ page }) => {
    await stubWindowOpen(page);
    await openRunDetail(page, RUN_TASK);

    const runId = runIdByTask(RUN_TASK);
    await page.getByRole("button", { name: "Open vscode" }).click();
    await expect
      .poll(async () => (await page.evaluate(() => (window as unknown as { __opened: string[] }).__opened)).length)
      .toBe(1);

    const opened = (await page.evaluate(() => (window as unknown as { __opened: string[] }).__opened))[0];
    const url = new URL(opened);
    // A DIFFERENT origin than the console's — the server's advertised one, never
    // one the console built from window.location.
    expect(url.origin).not.toBe(new URL(page.url()).origin);
    expect(url.pathname).toBe("/__wardyn/enter");
    expect(url.searchParams.get("run")).toBe(runId);
    expect(url.searchParams.get("app")).toBe("vscode");
    // A real single-use ticket from POST /runs/{id}/attach-ticket, not a
    // placeholder the template left behind.
    expect(url.searchParams.get("ticket") ?? "").not.toBe("{ticket}");
    expect((url.searchParams.get("ticket") ?? "").length).toBeGreaterThan(16);
  });

  test("a run that declares nothing says so, and names the policy field", async ({ page }) => {
    // The seeded run DOES declare one, so strip it on the wire: a run whose
    // effective policy names no app is the state a fresh deployment is in, and
    // the console must say which policy field turns it on rather than show a
    // dead lane. Stripped rather than seeded as a SECOND running run — the
    // seeded run count is load-bearing for the runs/ recording specs.
    const stripped = runIdByTask(RUN_TASK);
    await page.route(`**/api/v1/runs/${stripped}`, async (route) => {
      const res = await route.fetch();
      const body = await res.json();
      delete body.ui_apps;
      await route.fulfill({ response: res, body: JSON.stringify(body) });
    });
    await openRunDetail(page, RUN_TASK);
    await expect(page.getByText(NO_APPS_LINE)).toBeVisible();
    await expect(page.getByText("ui_apps", { exact: true })).toBeVisible();
    await expect(page.getByRole("button", { name: /^Open / })).toHaveCount(0);
  });

  test("with the gateway off the lane names the env var that turns it on", async ({ page }) => {
    // Serve a /healthz with the ui_sandbox block removed — exactly what a
    // deployment that never set WARDYN_UI_SANDBOX_LISTEN returns.
    await page.route("**/healthz", async (route) => {
      const res = await route.fetch();
      const body = await res.json();
      delete body.ui_sandbox;
      await route.fulfill({ response: res, body: JSON.stringify(body) });
    });
    await openRunDetail(page, RUN_TASK);

    await expect(page.getByText(OFF_LINE)).toBeVisible();
    await expect(page.getByText("WARDYN_UI_SANDBOX_LISTEN")).toBeVisible();
    await expect(page.getByRole("button", { name: /^Open / })).toHaveCount(0);
  });
});
