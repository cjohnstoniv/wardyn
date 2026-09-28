/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, sql, consoleAPI } from "./fixtures";
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

async function openRunDetail(page: Page, task: string): Promise<void> {
  await page.goto(`/runs/${runIdByTask(task)}`);
  await expect(page.getByRole("heading", { name: task, level: 1 })).toBeVisible();
  // The card is the last widget in the live rail, so it may be below its
  // container's fold.
  await page.getByText("UI apps", { exact: true }).scrollIntoViewIfNeeded();
}

// submitEnter drives the enter hand-off from page as a real form navigation
// (what the console's Open does), and returns the gateway's status for it.
async function submitEnter(page: Page, action: string, fields: Record<string, string>): Promise<number> {
  const response = page
    .context()
    .waitForEvent("response", (r) => r.url() === action && r.request().method() === "POST");
  await page.evaluate(
    ({ action, fields }) => {
      const form = document.createElement("form");
      form.method = "POST";
      form.action = action;
      form.target = "_blank";
      for (const [name, value] of Object.entries(fields)) {
        const input = document.createElement("input");
        input.type = "hidden";
        input.name = name;
        input.value = value;
        form.appendChild(input);
      }
      document.body.appendChild(form);
      form.submit();
    },
    { action, fields },
  );
  return (await response).status();
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

  // #1220: Open POSTs the ticket via a hidden auto-submitted form — the
  // console never puts it in a URL at all now. Intercepted rather than
  // stubbed (window.open is gone from this path): the popup's own first
  // request IS the assertion, and it is fulfilled locally rather than let
  // through, since the `none` runner behind this backend has no sandbox to
  // relay into (a real redeem-and-303 would just 409 here).
  test("Open mints a ticket and POSTs it to the gateway's own origin, never in a URL", async ({ page, context }) => {
    await openRunDetail(page, RUN_TASK);
    const runId = runIdByTask(RUN_TASK);

    let captured: { url: string; postData: string | null; cookie: string } | null = null;
    await context.route("**/__wardyn/enter", async (route) => {
      const headers = await route.request().allHeaders();
      captured = { url: route.request().url(), postData: route.request().postData(), cookie: headers["cookie"] ?? "" };
      await route.fulfill({ status: 200, contentType: "text/plain", body: "stopped for the test" });
    });

    const [popup] = await Promise.all([
      context.waitForEvent("page"),
      page.getByRole("button", { name: "Open vscode" }).click(),
    ]);
    await popup.waitForLoadState("domcontentloaded").catch(() => {});

    expect(captured).not.toBeNull();
    const { url: openedURL, postData, cookie } = captured!;
    const url = new URL(openedURL);
    // A DIFFERENT origin than the console's — the server's advertised one,
    // never one the console built from window.location.
    expect(url.origin).not.toBe(new URL(page.url()).origin);
    expect(url.pathname).toBe("/__wardyn/enter");
    // The whole point of #1220: no query string at all on this request.
    expect(url.search).toBe("");
    const form = new URLSearchParams(postData ?? "");
    expect(form.get("run")).toBe(runId);
    expect(form.get("app")).toBe("vscode");
    // A real single-use ticket from POST /runs/{id}/attach/ticket, not a
    // placeholder the template left behind, and it travels in the form body.
    expect(form.get("ticket") ?? "").not.toBe("{ticket}");
    expect((form.get("ticket") ?? "").length).toBeGreaterThan(16);
    // #1241: Open bound the ticket first, and the new tab's POST carries that
    // HttpOnly binding cookie on the gateway's origin.
    expect(cookie).toMatch(/wardyn_ui_bind_[0-9a-f]{16}=[0-9a-f]{64}/);
    await popup.close();
  });

  // #1241: a ticket minted and bound in one browser is refused in another — a
  // page pushing someone else's browser through the hand-off gets a 403 — and
  // that refusal does not spend it. The browser that bound it gets past the
  // ticket check: 409 here, because the `none` runner has no sandbox.
  test("a ticket is refused in a browser that did not bind it, and accepted in the one that did", async ({ page, browser }) => {
    await openRunDetail(page, RUN_TASK);
    const runId = runIdByTask(RUN_TASK);
    const health = JSON.parse((await consoleAPI(page, "GET", "/healthz")).text);
    const enterURL: string = health.ui_sandbox.enter_post_url;
    const bindURL: string = health.ui_sandbox.bind_url;
    const mint = await consoleAPI(page, "POST", `/api/v1/runs/${runId}/attach/ticket`);
    expect(mint.status, mint.text).toBe(200);
    const ticket: string = JSON.parse(mint.text).ticket;
    const fields = { run: runId, app: "vscode", ticket };

    const other = await browser.newContext();
    const victim = await other.newPage();
    await victim.setContent("<html><body></body></html>");
    expect(await submitEnter(victim, enterURL, fields)).toBe(403);
    await other.close();

    const bound = await page.evaluate(
      async ({ url, ticket }) =>
        (await fetch(url, { method: "POST", credentials: "include", body: new URLSearchParams({ ticket }) })).status,
      { url: bindURL, ticket },
    );
    expect(bound).toBe(204);
    expect(await submitEnter(page, enterURL, fields)).toBe(409);
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
