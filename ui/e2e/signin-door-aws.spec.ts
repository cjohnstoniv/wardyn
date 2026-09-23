/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { Page, WebSocketRoute } from "@playwright/test";
import { test, expect, gotoConsole, mockMemberRole, navToRoute } from "./fixtures";
import { attachModeFrame, stubAttachSocket, stubAttachTicket } from "./attach-stub";
import { SIGNIN_PROGRESS } from "../src/app/components/screens/settings/login-pane-copy";
import { MODEL_ACCESS_BANNER } from "../src/app/components/wardyn/model-access-copy";
import { AGENTS } from "../src/app/lib/workspace-providers-copy";

// The AWS sign-in door (#628, the approved sign-in progress packet), every
// drawn state, opened from the model-access strip the way a person in the User
// view opens it.
//
// HERMETIC, and why: this daemon runs `-runner none` (scripts/e2e-backend.sh),
// so no login run ever starts. The launch POST, the login run's reads, its
// kill, the attach ticket and the attach socket are all stubbed — the same
// technique attach-stub.ts documents for the cockpit — and the run the stub
// serves is MUTABLE, so one case can walk the door through the substrate's own
// answers (no reason, Pulling, RUNNING) the way a cold start does. What is real
// is the pane's own machine and the browser's own popup handling.

const DEVICE_URL = "https://device.sso.us-east-1.amazonaws.com/?user_code=KHDW-PQRS";
const PULL_FAILED = "agent: ErrImagePull: rpc error: code = NotFound desc = failed to pull and unpack image";

type LoginRun = Record<string, unknown>;

interface Door {
  /** What the next GET of the login run answers. */
  setRun(run: LoginRun): void;
  /** The attach socket, once the pane has opened it. */
  socket(): Promise<WebSocketRoute>;
  /** Flip /setup/status to show this run's capture stored. */
  markCaptured(): void;
  kills: string[];
  launches: () => number;
  /** Every tab the page opened, so a case can say none did. */
  tabs: Page[];
}

async function openAwsDoor(page: Page): Promise<Door> {
  await mockMemberRole(page);
  let captured = false;
  let base: Record<string, unknown> | null = null;
  // Cached once and composed per read: the shell polls this endpoint, and a
  // real round trip per match races Playwright disposing an in-flight response
  // (model-access-banner.spec.ts's own reason).
  await page.route("**/api/v1/setup/status*", async (route) => {
    if (!base) base = (await (await route.fetch()).json()) as Record<string, unknown>;
    const body: Record<string, unknown> = { ...base };
    body.model_access = { state: "not_configured", mechanism: "bedrock_sso", action: AGENTS.SIGN_IN_AWS };
    body.harnesses = ((base.harnesses ?? []) as { id: string }[]).map((h) =>
      h.id === "claude-code" ? { ...h, enabled: true, mechanism: "bedrock_sso", credential_source: "per_user" } : h,
    );
    if (captured) body.harness = [{ provider: "aws", captured: true, source_run_id: runIds[runIds.length - 1] }];
    await route.fulfill({ json: body });
  });

  const runIds: string[] = [];
  await page.route("**/api/v1/setup/harness-login", async (route) => {
    const id = `3f1b7c26-0000-4000-8000-${String(runIds.length + 1).padStart(12, "0")}`;
    runIds.push(id);
    await stubAttachTicket(page, id);
    await route.fulfill({ json: { run_id: id, state: "PENDING" } });
  });
  let run: LoginRun = { state: "PENDING" };
  await page.route(/\/api\/v1\/runs\/3f1b7c26-0000-4000-8000-\d{12}$/, async (route) => {
    const id = new URL(route.request().url()).pathname.split("/").pop();
    await route.fulfill({ json: { id, task: "harness login", interactive: true, ...run } });
  });
  const kills: string[] = [];
  await page.route(/\/api\/v1\/runs\/3f1b7c26-0000-4000-8000-\d{12}\/kill$/, async (route) => {
    kills.push(new URL(route.request().url()).pathname.split("/").at(-2) ?? "");
    await route.fulfill({ status: 202, json: {} });
  });
  let resolveSocket: (ws: WebSocketRoute) => void = () => {};
  const socketP = new Promise<WebSocketRoute>((r) => (resolveSocket = r));
  await stubAttachSocket(page, (_n, ws) => {
    ws.send(attachModeFrame(false));
    resolveSocket(ws);
  });

  const tabs: Page[] = [];
  page.context().on("page", (p) => tabs.push(p));

  await gotoConsole(page);
  await navToRoute(page, "/runs");
  await page.getByRole("button", { name: AGENTS.SIGN_IN_AWS }).click();
  await expect(page.getByRole("heading", { name: MODEL_ACCESS_BANNER.DIALOG_TITLE })).toBeVisible();
  await page.getByRole("button", { name: /start login/i }).click();

  return {
    setRun: (r) => (run = r),
    socket: () => socketP,
    markCaptured: () => (captured = true),
    kills,
    launches: () => runIds.length,
    tabs,
  };
}

/** The step row holding `label`, for its data-state. */
function step(page: Page, label: string) {
  return page.getByTestId("signin-progress").getByRole("listitem").filter({ hasText: label });
}

/** Up to the ready state: the sandbox is RUNNING and has printed its link. */
async function reachReady(page: Page, door: Door): Promise<void> {
  door.setRun({ state: "RUNNING" });
  const ws = await door.socket();
  ws.send(Buffer.from(`Open the following URL in a browser:\r\n${DEVICE_URL}\r\n`));
  await expect(page.getByTestId("signin-ready")).toBeVisible();
}

test.describe("the AWS sign-in door (#628)", () => {
  test("states 1, 2b, 3 and 4: the door walks the cold start, and Start opens no tab", async ({ page }) => {
    const door = await openAwsDoor(page);

    // 1 · Starting the sandbox — no hint, only the step.
    await expect(step(page, SIGNIN_PROGRESS.STEP_START)).toHaveAttribute("data-state", "active");
    await expect(step(page, SIGNIN_PROGRESS.STEP_DOWNLOAD)).toHaveAttribute("data-state", "pending");
    await expect(step(page, SIGNIN_PROGRESS.STEP_WAIT("AWS"))).toHaveAttribute("data-state", "pending");

    // 2b · Downloading, progress unknown: no runner reports pull progress, so
    // the plain sentence stands in for a number the door does not have.
    door.setRun({ state: "STARTING", status_detail: "image: Pulling: wardyn/agent-aws-sso:local", status_reason: "Pulling" });
    await expect(step(page, SIGNIN_PROGRESS.STEP_DOWNLOAD_ACTIVE)).toHaveAttribute("data-state", "active");
    await expect(step(page, SIGNIN_PROGRESS.STEP_START)).toHaveAttribute("data-state", "done");
    await expect(page.getByText(SIGNIN_PROGRESS.DOWNLOAD_HINT)).toBeVisible();

    // 3 · Waiting for AWS.
    door.setRun({ state: "RUNNING" });
    await door.socket();
    await expect(step(page, SIGNIN_PROGRESS.STEP_WAIT("AWS"))).toHaveAttribute("data-state", "active");
    await expect(step(page, SIGNIN_PROGRESS.STEP_DOWNLOAD)).toHaveAttribute("data-state", "done");
    await expect(page.getByText(SIGNIN_PROGRESS.WAIT_HINT("AWS"))).toBeVisible();

    // 4 · Ready: the button, the code beside it, and the copy-link fallback.
    (await door.socket()).send(Buffer.from(`${DEVICE_URL}\r\n`));
    const ready = page.getByTestId("signin-ready");
    await expect(ready.getByRole("button", { name: SIGNIN_PROGRESS.OPEN("AWS") })).toBeVisible();
    await expect(page.getByTestId("signin-device-code")).toHaveText("KHDW-PQRS");
    await expect(ready).toContainText(`${SIGNIN_PROGRESS.COPY_LEAD} ${SIGNIN_PROGRESS.COPY_LINK}`);
    await expect(ready).toContainText(DEVICE_URL);
    await expect(step(page, SIGNIN_PROGRESS.STEP_WAIT("AWS"))).toHaveAttribute("data-state", "done");

    // No about:blank tab, at any point of the wait.
    expect(door.tabs).toHaveLength(0);
  });

  test("state 5: Open opens one tab at AWS's page with no opener, then Reopen opens it again", async ({ page, context }) => {
    await context.route("https://device.sso.us-east-1.amazonaws.com/**", (route) =>
      route.fulfill({ contentType: "text/html", body: "<title>AWS device sign-in</title>" }),
    );
    const door = await openAwsDoor(page);
    await reachReady(page, door);
    expect(door.tabs).toHaveLength(0);

    const popup = page.waitForEvent("popup");
    await page.getByRole("button", { name: SIGNIN_PROGRESS.OPEN("AWS") }).click();
    const tab = await popup;
    await expect(tab).toHaveURL(DEVICE_URL);
    // Finding 7a: the provider's page holds no reference back into the console.
    expect(await tab.evaluate(() => window.opener)).toBeNull();

    const opened = page.getByTestId("signin-tab-open");
    await expect(opened).toContainText(SIGNIN_PROGRESS.TAB_OPEN("AWS"));
    await expect(page.getByTestId("signin-device-code")).toHaveText("KHDW-PQRS");
    await expect(page.getByTestId("signin-progress")).toHaveCount(0);

    const again = page.waitForEvent("popup");
    await opened.getByRole("button", { name: SIGNIN_PROGRESS.REOPEN }).click();
    await expect(await again).toHaveURL(DEVICE_URL);
    expect(door.tabs).toHaveLength(2);
  });

  test("a browser that blocks the popup keeps the ready state and its copy-link fallback", async ({ page }) => {
    await page.addInitScript(() => {
      window.open = () => null;
    });
    const door = await openAwsDoor(page);
    await reachReady(page, door);
    await page.getByRole("button", { name: SIGNIN_PROGRESS.OPEN("AWS") }).click();
    await expect(page.getByTestId("signin-tab-open")).toHaveCount(0);
    await expect(page.getByTestId("signin-ready").getByRole("button", { name: SIGNIN_PROGRESS.COPY_LINK })).toBeVisible();
  });

  test("state 6: a stored sign-in closes the door with the existing toast", async ({ page }) => {
    const door = await openAwsDoor(page);
    await reachReady(page, door);
    door.markCaptured();
    (await door.socket()).send(Buffer.from("wardyn: aws sso credential captured\r\n"));
    await expect(page.getByText(MODEL_ACCESS_BANNER.SIGNED_IN_TOAST)).toBeVisible();
    await expect(page.getByRole("heading", { name: MODEL_ACCESS_BANNER.DIALOG_TITLE })).toHaveCount(0);
    expect(door.tabs).toHaveLength(0);
  });

  test("state 7: a failed image pull shows the server's reason verbatim, and Retry starts a fresh sandbox", async ({ page }) => {
    const door = await openAwsDoor(page);
    door.setRun({ state: "STARTING", status_detail: PULL_FAILED, status_reason: "ErrImagePull" });

    await expect(page.getByTestId("harness-login-pane").getByRole("alert")).toHaveText(PULL_FAILED);
    await expect(step(page, SIGNIN_PROGRESS.STEP_DOWNLOAD_FAILED)).toHaveAttribute("data-state", "failed");
    await expect(step(page, SIGNIN_PROGRESS.STEP_START)).toHaveAttribute("data-state", "done");
    await expect(page.getByTestId("signin-progress").getByRole("listitem")).toHaveCount(2);

    door.setRun({ state: "PENDING" });
    await page.getByRole("button", { name: SIGNIN_PROGRESS.RETRY }).click();
    await expect.poll(() => door.launches()).toBe(2);
    // The stuck sandbox is still STARTING; Retry stops it rather than leaving it behind.
    expect(door.kills).toContain("3f1b7c26-0000-4000-8000-000000000001");
    await expect(page.getByTestId("harness-login-pane").getByRole("alert")).toHaveCount(0);
    await expect(step(page, SIGNIN_PROGRESS.STEP_START)).toHaveAttribute("data-state", "active");
  });

  test("state 8: Cancel stops the sandbox and the strip goes back to what it showed", async ({ page }) => {
    const door = await openAwsDoor(page);
    await expect(page.getByTestId("signin-progress")).toBeVisible();
    await page.getByTestId("login-sandbox-starting").getByRole("button", { name: SIGNIN_PROGRESS.CANCEL }).click();

    await expect(page.getByRole("heading", { name: MODEL_ACCESS_BANNER.DIALOG_TITLE })).toHaveCount(0);
    await expect.poll(() => door.kills.length).toBeGreaterThanOrEqual(1);
    await expect(page.getByText(MODEL_ACCESS_BANNER.NOT_SIGNED_IN)).toBeVisible();
    await expect(page.getByRole("button", { name: AGENTS.SIGN_IN_AWS })).toBeVisible();
    await expect(page.getByRole("button", { name: MODEL_ACCESS_BANNER.NOT_NOW })).toBeVisible();
    expect(door.tabs).toHaveLength(0);
  });
});
