/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { Page, WebSocketRoute } from "@playwright/test";
import { test, expect, sql, consoleAPI, gotoConsole, navToRoute } from "./fixtures";
import { NR_MACHINE } from "../src/app/components/wardyn/copy/new-run/machine";
import { NR_SHAPE } from "../src/app/components/wardyn/copy/new-run/shape";
import { NR_START } from "../src/app/components/wardyn/copy/new-run/start";
import { DESKTOP } from "../src/app/components/wardyn/copy/desktop";

// ============================================================================
// A Linux desktop VM (mocks M5-Revised and M6): the New Run machine-kind picker, the run page's Desktop
// block, and the gateway's refusal page for a desktop link. The `none` runner behind this backend has no VM,
// so the seeded RUNNING run is shown as a VM on the wire and the stream itself is proven by the Go gateway
// tests and the hosted VM conformance job.
// ============================================================================

const RUN_TASK = "e2e fixture 2";

function runIdByTask(task: string): string {
  const id = sql(`SELECT id FROM agent_runs WHERE task = '${task}' LIMIT 1`);
  expect(id, `no seeded run with task ${task}`).toMatch(/^[0-9a-f-]{36}$/);
  return id;
}

/** The seeded run's page, with the run shown as a Linux desktop VM. */
async function openVMRun(page: Page): Promise<string> {
  const runId = runIdByTask(RUN_TASK);
  await page.route(`**/api/v1/runs/${runId}`, async (route) => {
    const res = await route.fetch();
    const body = await res.json();
    await route.fulfill({ response: res, body: JSON.stringify({ ...body, machine_kind: "linux_vm" }) });
  });
  await page.goto(`/runs/${runId}`);
  await expect(page.getByRole("heading", { name: RUN_TASK, level: 1 })).toBeVisible();
  return runId;
}

test.describe("New Run — machine kind", () => {
  test("a virtual machine is Linux, the VM is its barrier, and the container barriers come back on Container", async ({ page }) => {
    await gotoConsole(page);
    await navToRoute(page, "/runs/new");
    await page.getByRole("button", { name: NR_START.GUIDED_FOOT }).click();
    await page.getByRole("button", { name: NR_SHAPE.ENV_TITLE, exact: true }).click();

    const kind = page.getByRole("group", { name: NR_MACHINE.LABEL });
    await expect(kind.getByRole("button", { name: NR_MACHINE.CONTAINER })).toHaveAttribute("aria-pressed", "true");
    await expect(page.getByRole("radiogroup", { name: "Barrier" })).toBeVisible();

    await kind.getByRole("button", { name: NR_MACHINE.VM }).click();
    await expect(kind.getByText(NR_MACHINE.VM_HELP)).toBeVisible();
    await expect(page.getByRole("group", { name: NR_MACHINE.OS_LABEL }).getByRole("button", { name: NR_MACHINE.OS_LINUX })).toHaveAttribute("aria-pressed", "true");
    await expect(page.getByTestId("nr-vm-barrier")).toContainText(NR_MACHINE.BARRIER_VM);
    await expect(page.getByRole("region", { name: NR_MACHINE.CHANGES_TITLE })).toBeVisible();
    await expect(page.getByRole("radiogroup", { name: "Barrier" })).toHaveCount(0);

    await kind.getByRole("button", { name: NR_MACHINE.CONTAINER }).click();
    await expect(page.getByRole("radiogroup", { name: "Barrier" })).toBeVisible();
    await expect(page.getByTestId("nr-vm-barrier")).toHaveCount(0);
  });
});

test.describe("Run detail — Desktop", () => {
  test("Open desktop POSTs a bound ticket for the built-in desktop app to the gateway's own origin", async ({ page, context }) => {
    const runId = await openVMRun(page);
    const desk = page.getByRole("region", { name: DESKTOP.title });
    await desk.scrollIntoViewIfNeeded();
    await expect(desk.getByText(DESKTOP.ready, { exact: true })).toBeVisible();
    await expect(desk.getByText(DESKTOP.kind)).toBeVisible();

    let captured: { url: string; postData: string | null; cookie: string } | null = null;
    await context.route("**/__wardyn/enter", async (route) => {
      const headers = await route.request().allHeaders();
      captured = { url: route.request().url(), postData: route.request().postData(), cookie: headers["cookie"] ?? "" };
      await route.fulfill({ status: 200, contentType: "text/plain", body: "stopped for the test" });
    });
    const [popup] = await Promise.all([context.waitForEvent("page"), desk.getByRole("button", { name: "Open desktop" }).click()]);
    await popup.waitForLoadState("domcontentloaded").catch(() => {});

    expect(captured).not.toBeNull();
    const { url, postData, cookie } = captured!;
    expect(new URL(url).origin).not.toBe(new URL(page.url()).origin);
    expect(new URL(url).search).toBe("");
    const form = new URLSearchParams(postData ?? "");
    expect(form.get("run")).toBe(runId);
    expect(form.get("app")).toBe("__desktop");
    expect((form.get("ticket") ?? "").length).toBeGreaterThan(16);
    expect(cookie).toMatch(/wardyn_ui_bind_[0-9a-f]{16}=[0-9a-f]{64}/);
    await popup.close();
  });

  test("with the gateway off the Desktop block says so and offers no button", async ({ page }) => {
    await page.route("**/healthz", async (route) => {
      const res = await route.fetch();
      const body = await res.json();
      delete body.ui_sandbox;
      await route.fulfill({ response: res, body: JSON.stringify(body) });
    });
    await openVMRun(page);
    const desk = page.getByRole("region", { name: DESKTOP.title });
    await desk.scrollIntoViewIfNeeded();
    await expect(desk.getByText(DESKTOP.statusOff, { exact: true })).toBeVisible();
    await expect(desk.getByText(/^Off on this deployment\. It relays the desktop of a sandbox/)).toBeVisible();
    await expect(desk.getByRole("button")).toHaveCount(0);
  });

  test("a desktop link opened in a browser that did not bind it gets the refusal page, with the gateway's words", async ({ page, browser }) => {
    const runId = runIdByTask(RUN_TASK);
    await gotoConsole(page);
    const health = JSON.parse((await consoleAPI(page, "GET", "/healthz")).text);
    const mint = await consoleAPI(page, "POST", `/api/v1/runs/${runId}/attach/ticket`);
    expect(mint.status, mint.text).toBe(200);
    const fields = { run: runId, app: "__desktop", ticket: JSON.parse(mint.text).ticket as string };

    const other = await browser.newContext();
    const tab = await other.newPage();
    await tab.setContent("<html><body></body></html>");
    const answer = tab.waitForResponse((r) => r.url() === health.ui_sandbox.enter_post_url && r.request().method() === "POST");
    await tab.evaluate(
      ({ action, fields }) => {
        const form = document.createElement("form");
        form.method = "POST";
        form.action = action;
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
      { action: health.ui_sandbox.enter_post_url as string, fields },
    );
    expect((await answer).status()).toBe(403);
    const card = tab.getByRole("region", { name: DESKTOP.linkBlocked });
    await expect(card).toBeVisible();
    await expect(card.getByText(DESKTOP.linkBody)).toBeVisible();
    await expect(card.getByTestId("desktop-refused-said")).toHaveText("invalid, expired, or already-used attach ticket");
    await other.close();
  });

  test("a Close the gateway did not store keeps the desktop open, says so, and leaves Close to try again", async ({ page }) => {
    const v = await openViewerWithFailingClose(page);
    const close = page.getByRole("button", { name: DESKTOP.close });
    await close.click();
    await expect(page.getByRole("alert")).toHaveText("Close didn't go through, so this desktop is still open. Try again.");
    expect(DESKTOP.closeFailed).toBe("Close didn't go through, so this desktop is still open. Try again.");
    await expect(close).toBeEnabled();
    expect(v.closes()).toBe(1);
    expect(page.isClosed()).toBe(false);
  });

  test("a stream that drops while a Close is in flight reconnects, and the failed Close still says so", async ({ page }) => {
    const v = await openViewerWithFailingClose(page, (sockets) => sockets[0].close({ code: 4000, reason: "dropped" }));
    const close = page.getByRole("button", { name: DESKTOP.close });
    await close.click();
    await expect(page.getByRole("alert")).toHaveText("Close didn't go through, so this desktop is still open. Try again.");
    await expect(close).toBeEnabled();
    await expect.poll(() => v.dials()).toBe(2);
  });
});

/**
 * The gateway's own viewer page booted as an open desktop: the `none` runner has no VM to open a session on,
 * so the page (its refusal, served without a session) gets an open boot, its stream is held open here, and
 * its Close is answered 503, after `beforeAnswer` when given.
 */
async function openViewerWithFailingClose(page: Page, beforeAnswer?: (sockets: WebSocketRoute[]) => Promise<void>) {
  const runId = runIdByTask(RUN_TASK);
  await gotoConsole(page);
  const health = JSON.parse((await consoleAPI(page, "GET", "/healthz")).text);
  const viewer = new URL(`../r/${runId}/__desktop/`, health.ui_sandbox.enter_post_url as string);
  const path = viewer.pathname.replace(/\/$/, "");
  const boot = { state: "open", expires: 4102444800, rfb: `${path}/__rfb`, close: `${path}/__close` };
  await page.route(viewer.href, async (route) => {
    const res = await route.fetch();
    const body = (await res.text()).replace(/data-wardyn-desktop="[^"]*"/, `data-wardyn-desktop="${JSON.stringify(boot).replace(/"/g, "&quot;")}"`);
    await route.fulfill({ response: res, status: 200, body });
  });
  const sockets: WebSocketRoute[] = [];
  await page.routeWebSocket(`**${path}/__rfb`, (ws) => {
    sockets.push(ws);
  });
  let closes = 0;
  await page.route(`**${path}/__close`, async (route) => {
    closes += 1;
    await beforeAnswer?.(sockets);
    await route.fulfill({ status: 503, contentType: "application/json", body: JSON.stringify({ error: "could not close this desktop session; try again" }) });
  });
  await page.goto(viewer.href);
  await expect.poll(() => sockets.length).toBe(1);
  return { closes: () => closes, dials: () => sockets.length };
}
