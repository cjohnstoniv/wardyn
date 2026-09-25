/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { Page, WebSocketRoute } from "@playwright/test";
import { test, expect, gotoConsole, navToRoute } from "./fixtures";
import { attachModeFrame, stubAttachSocket, stubAttachTicket } from "./attach-stub";
import { SIGNIN_PROGRESS } from "../src/app/components/screens/settings/login-pane-copy";

// The Claude sign-in door (#628). The packet draws it as the AWS door's
// states 1-8 unchanged in shape, with no device code: Claude's flow is a link
// and an approval, and the code it hands back is pasted into this dialog.
// Opened from Settings' model provider card, which opens the shell's one door
// (#544) rather than mounting its own.
//
// Hermetic for the reason signin-door-aws.spec.ts gives: `-runner none` never
// starts a login run, so the run, its kill, the attach and the token write are
// stubbed, and the run the stub serves is mutable.

const OAUTH_URL =
  "https://claude.ai/oauth/authorize?code=true&client_id=e2e-client&response_type=code&redirect_uri=https%3A%2F%2Fconsole.anthropic.com%2Foauth%2Fcode%2Fcallback&state=e2e-state";
const TOKEN = "sk-ant-oat01-" + "A".repeat(60);
const PULL_FAILED = "agent: ImagePullBackOff: Back-off pulling image \"wardyn/agent-claude-code:local\"";

type LoginRun = Record<string, unknown>;

interface Door {
  setRun(run: LoginRun): void;
  socket(): Promise<WebSocketRoute>;
  kills: string[];
  launches: () => number;
  tokenWrites: () => number;
  tabs: Page[];
}

async function openClaudeDoor(page: Page): Promise<Door> {
  let base: Record<string, unknown> | null = null;
  // No subscription of any kind — neither a managed capture nor this host's
  // own resident Claude CLI login, which the e2e host may well have — and a
  // deployment that allows one: the lane's "Sign in" button is on screen.
  await page.route("**/api/v1/setup/status*", async (route) => {
    if (!base) base = (await (await route.fetch()).json()) as Record<string, unknown>;
    const auth = { ...((base.auth ?? {}) as Record<string, unknown>), shared_subscription_allowed: true };
    const providers = ((base.providers ?? []) as { tool?: string }[]).map((p) =>
      p.tool === "claude" ? { ...p, logged_in: false } : p,
    );
    const harness = ((base.harness ?? []) as { provider?: string }[]).filter((h) => h.provider !== "anthropic");
    await route.fulfill({ json: { ...base, auth, providers, harness } });
  });
  const runIds: string[] = [];
  await page.route("**/api/v1/setup/harness-login", async (route) => {
    const id = `3f1b7c26-0000-4000-8000-${String(runIds.length + 101).padStart(12, "0")}`;
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
  let tokenWrites = 0;
  await page.route("**/api/v1/setup/harness-credential/anthropic", async (route) => {
    tokenWrites++;
    await route.fulfill({ json: {} });
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
  await navToRoute(page, "/admin/settings");
  await page.locator("#lane-subscription").click();
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Sign in to Claude" })).toBeVisible();
  await page.getByRole("button", { name: /start login/i }).click();

  return {
    setRun: (r) => (run = r),
    socket: () => socketP,
    kills,
    launches: () => runIds.length,
    tokenWrites: () => tokenWrites,
    tabs,
  };
}

function step(page: Page, label: string) {
  return page.getByTestId("signin-progress").getByRole("listitem").filter({ hasText: label });
}

async function reachReady(page: Page, door: Door): Promise<void> {
  door.setRun({ state: "RUNNING" });
  (await door.socket()).send(Buffer.from(`Browser didn't open? Use the url below to sign in:\r\n${OAUTH_URL}\r\n`));
  await expect(page.getByTestId("signin-ready")).toBeVisible();
}

test.describe("the Claude sign-in door (#628)", () => {
  test("states 1, 2b, 3 and 4: the door walks the cold start, and Start opens no tab", async ({ page }) => {
    const door = await openClaudeDoor(page);

    await expect(step(page, SIGNIN_PROGRESS.STEP_START)).toHaveAttribute("data-state", "active");
    await expect(step(page, SIGNIN_PROGRESS.STEP_WAIT("Claude"))).toHaveAttribute("data-state", "pending");

    door.setRun({ state: "STARTING", status_detail: "image: Pulling: wardyn/agent-claude-code:local", status_reason: "Pulling" });
    await expect(step(page, SIGNIN_PROGRESS.STEP_DOWNLOAD_ACTIVE)).toHaveAttribute("data-state", "active");
    await expect(page.getByText(SIGNIN_PROGRESS.DOWNLOAD_HINT)).toBeVisible();

    door.setRun({ state: "RUNNING" });
    await door.socket();
    await expect(step(page, SIGNIN_PROGRESS.STEP_WAIT("Claude"))).toHaveAttribute("data-state", "active");
    await expect(page.getByText(SIGNIN_PROGRESS.WAIT_HINT("Claude"))).toBeVisible();

    (await door.socket()).send(Buffer.from(`${OAUTH_URL}\r\n`));
    const ready = page.getByTestId("signin-ready");
    await expect(ready.getByRole("button", { name: SIGNIN_PROGRESS.OPEN("Claude") })).toBeVisible();
    await expect(ready).toContainText(`${SIGNIN_PROGRESS.COPY_LEAD} ${SIGNIN_PROGRESS.COPY_LINK}`);
    // No device code, and the ~250-character link is copied, never printed.
    await expect(page.getByTestId("signin-device-code")).toHaveCount(0);
    await expect(ready).not.toContainText("claude.ai/oauth");
    expect(door.tabs).toHaveLength(0);
  });

  test("state 5: Open opens one tab at Claude's page with no opener, then Reopen opens it again", async ({ page, context }) => {
    await context.route("https://claude.ai/oauth/**", (route) =>
      route.fulfill({ contentType: "text/html", body: "<title>Claude sign-in</title>" }),
    );
    const door = await openClaudeDoor(page);
    await reachReady(page, door);

    const popup = page.waitForEvent("popup");
    await page.getByRole("button", { name: SIGNIN_PROGRESS.OPEN("Claude") }).click();
    const tab = await popup;
    await expect(tab).toHaveURL(OAUTH_URL);
    expect(await tab.evaluate(() => window.opener)).toBeNull();

    const opened = page.getByTestId("signin-tab-open");
    await expect(opened).toContainText(SIGNIN_PROGRESS.TAB_OPEN("Claude"));
    // The code Claude hands back is still pasted here.
    await expect(page.getByLabel("login code")).toBeVisible();

    const again = page.waitForEvent("popup");
    await opened.getByRole("button", { name: SIGNIN_PROGRESS.REOPEN }).click();
    await expect(await again).toHaveURL(OAUTH_URL);
  });

  test("state 6: the captured token is stored and the door closes", async ({ page }) => {
    const door = await openClaudeDoor(page);
    await reachReady(page, door);
    (await door.socket()).send(Buffer.from(`${TOKEN}\r\n`));
    await expect.poll(() => door.tokenWrites()).toBe(1);
    await expect(page.getByRole("heading", { name: "Sign in to Claude" })).toHaveCount(0);
    expect(door.tabs).toHaveLength(0);
  });

  test("state 7: a failed image pull shows the server's reason verbatim, and Retry starts a fresh sandbox", async ({ page }) => {
    const door = await openClaudeDoor(page);
    door.setRun({ state: "STARTING", status_detail: PULL_FAILED, status_reason: "ImagePullBackOff" });

    await expect(page.getByTestId("harness-login-pane").getByRole("alert")).toHaveText(PULL_FAILED);
    await expect(step(page, SIGNIN_PROGRESS.STEP_DOWNLOAD_FAILED)).toHaveAttribute("data-state", "failed");
    await expect(page.getByTestId("signin-progress").getByRole("listitem")).toHaveCount(2);

    door.setRun({ state: "PENDING" });
    await page.getByRole("button", { name: SIGNIN_PROGRESS.RETRY }).click();
    await expect.poll(() => door.launches()).toBe(2);
    expect(door.kills).toContain("3f1b7c26-0000-4000-8000-000000000101");
    await expect(step(page, SIGNIN_PROGRESS.STEP_START)).toHaveAttribute("data-state", "active");
  });

  test("state 8: Cancel stops the sandbox and the card goes back to what it showed", async ({ page }) => {
    const door = await openClaudeDoor(page);
    await page.getByTestId("login-sandbox-starting").getByRole("button", { name: SIGNIN_PROGRESS.CANCEL }).click();
    await expect(page.getByRole("heading", { name: "Sign in to Claude" })).toHaveCount(0);
    await expect.poll(() => door.kills.length).toBeGreaterThanOrEqual(1);
    await expect(page.getByRole("button", { name: "Sign in", exact: true })).toBeVisible();
    expect(door.tabs).toHaveLength(0);
  });
});
