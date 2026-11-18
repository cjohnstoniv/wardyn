/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { Locator, Page, WebSocketRoute } from "@playwright/test";
import { test, expect, gotoConsole, navTo, navToRoute } from "./fixtures";
import { attachModeFrame, findRunningFixture, stubAttachSocket, stubAttachTicket } from "./attach-stub";
import { SUPPORT_BANNER, SUPPORT_CONSENT, SUPPORT_HELPER, SUPPORT_REQUEST } from "../src/app/components/wardyn/copy/support";
import { DESKTOP_FILES, RUN_CLI, RUN_COCKPIT, UI_APPS_LANE } from "../src/app/components/wardyn/copy";
import { DESKTOP } from "../src/app/components/wardyn/copy/desktop";
import { INBOUND_APP } from "../src/app/components/wardyn/copy/ui-apps";
import { RUN_SYNC } from "../src/app/components/wardyn/copy/runners";

// Support sessions in the console (approved board SC): the helper's Request help dialog, the owner's consent
// screen (doors and minutes), Decline, End now, the banner each party reads, and nothing at all when
// /setup/status leaves support_sessions out. The hermetic backend has no second signed-in person, so the three
// answers the page reads are spliced: /setup/status (the switch), /me (who is looking) and GET /runs/{id}
// (whose run it is), and the five support routes are served here from one small state, as the server's
// internal/api/support_grant_test.go pins them.
const HELPER = "support-helper@e2e.example";
const OWNER = "support-owner@e2e.example";

type Grant = Record<string, unknown>;
interface World {
  open: Grant | null;
  ended: Grant | null;
  calls: { method: string; path: string; body: unknown }[];
}

async function spliceIdentity(page: Page, opts: { switchOn: boolean; viewer: string }): Promise<void> {
  let setup: Record<string, unknown> | null = null;
  await page.route("**/api/v1/setup/status*", async (route) => {
    if (!setup) {
      const response = await route.fetch();
      setup = await response.json();
      const features = (setup!.features ?? {}) as Record<string, unknown>;
      if (opts.switchOn) features.support_sessions = true;
      else delete features.support_sessions;
      setup!.features = features;
    }
    await route.fulfill({ json: setup! });
  });
  await page.route("**/api/v1/me", async (route) => {
    const response = await route.fetch();
    const json = await response.json();
    json.principal = opts.viewer;
    await route.fulfill({ response, json });
  });
  await page.route("**/api/v1/runs/*", async (route) => {
    if (route.request().method() !== "GET") return route.fallback();
    const response = await route.fetch();
    const json = await response.json();
    if (json.task === "e2e fixture 0") Object.assign(json, { created_by: OWNER, state: "RUNNING", operator_owned: false });
    await route.fulfill({ response, json });
  });
}

async function serveSupport(page: Page, world: World): Promise<void> {
  await page.route("**/api/v1/runs/*/support**", async (route) => {
    const req = route.request();
    const path = new URL(req.url()).pathname;
    const raw = req.postData();
    world.calls.push({ method: req.method(), path, body: raw ? JSON.parse(raw) : null });
    const verb = path.split("/").pop();
    if (req.method() === "GET") {
      return route.fulfill({
        json: { open: world.open, ended: world.ended, doors_offered: ["terminal", "commands", "files", "forward", "web_apps", "desktop"], recorded_doors: ["terminal"] },
      });
    }
    if (verb === "support") {
      const body = JSON.parse(raw ?? "{}");
      world.open = { grant: "g-e2e", helper: HELPER, reason: body.reason, doors_asked: body.doors, requested_at: new Date().toISOString(), state: "requested", recorded_doors: ["terminal"] };
      return route.fulfill({ status: 201, json: world.open });
    }
    const grant = world.open as Grant;
    if (verb === "accept") {
      const body = JSON.parse(raw ?? "{}");
      world.open = { ...grant, state: "active", doors: body.doors, minutes: body.minutes, expires_at: new Date(Date.now() + body.minutes * 60_000).toISOString() };
      return route.fulfill({ json: world.open });
    }
    world.ended = { ...grant, state: "ended", end_reason: verb === "decline" ? "declined" : "revoked", reason: undefined };
    world.open = null;
    return route.fulfill({ json: world.ended });
  });
}

async function openRun(page: Page): Promise<void> {
  await gotoConsole(page);
  await navTo(page, "Runs");
  await page.getByText("e2e fixture 0").click();
  await expect(page).toHaveURL(/\/runs\/[0-9a-f-]{8,}/);
}

// The helper's terminal: the seeded RUNNING fixture 2, made the owner's interactive run. Cache-and-serve, as
// attach-stub.ts's stubInteractiveRun does, since the run page polls this read. Registered after spliceIdentity,
// so it answers first for this one run.
async function helpedTerminalRun(page: Page): Promise<string> {
  const { id: runId } = await findRunningFixture(page);
  let cached: Record<string, unknown> | null = null;
  await page.route(`**/api/v1/runs/${runId}`, async (route) => {
    if (route.request().method() !== "GET") return route.fallback();
    if (!cached) {
      const response = await route.fetch();
      cached = { ...(await response.json()), interactive: true, state: "RUNNING", created_by: OWNER, operator_owned: false };
    }
    await route.fulfill({ json: cached! });
  });
  await stubAttachTicket(page, runId);
  return runId;
}

const world = (over: Partial<World> = {}): World => ({ open: null, ended: null, calls: [], ...over });
const requested = (): Grant => ({ grant: "g-e2e", helper: HELPER, reason: "The build keeps stalling.", doors_asked: ["terminal", "commands", "files"], requested_at: new Date().toISOString(), state: "requested", recorded_doors: ["terminal"] });

test.describe("Support sessions", () => {
  test("without support_sessions in setup the run page shows no Request help and reads no support route", async ({ page }) => {
    const w = world();
    await spliceIdentity(page, { switchOn: false, viewer: HELPER });
    await serveSupport(page, w);
    await openRun(page);
    await expect(page.getByTestId("run-summary-header")).toBeVisible();
    await expect(page.getByRole("button", { name: SUPPORT_REQUEST.TITLE })).toHaveCount(0);
    await expect(page.getByTestId("support-strip")).toHaveCount(0);
    expect(w.calls).toHaveLength(0);
  });

  test("a helper asks for two doors with a reason, and then waits for the owner", async ({ page }) => {
    const w = world();
    await spliceIdentity(page, { switchOn: true, viewer: HELPER });
    await serveSupport(page, w);
    await openRun(page);
    await page.getByRole("button", { name: SUPPORT_REQUEST.TITLE }).click();
    const dialog = page.getByTestId("support-request-dialog");
    await expect(dialog.getByRole("button", { name: SUPPORT_REQUEST.SEND })).toBeDisabled();
    await dialog.getByRole("checkbox", { name: /Terminal/ }).click();
    await dialog.getByRole("checkbox", { name: /Commands/ }).click();
    await dialog.getByRole("textbox", { name: SUPPORT_REQUEST.REASON }).fill("The build keeps stalling.");
    await expect(dialog.getByTestId("support-reason-count")).toHaveText(SUPPORT_REQUEST.COUNT(25));
    await dialog.getByRole("button", { name: SUPPORT_REQUEST.SEND }).click();
    await expect(page.getByTestId("support-requested").getByText(SUPPORT_BANNER.WAITING(OWNER))).toBeVisible();
    expect(w.calls.find((c) => c.method === "POST")?.body).toEqual({ doors: ["terminal", "commands"], reason: "The build keeps stalling." });
    await expect(page.getByRole("button", { name: SUPPORT_REQUEST.TITLE })).toHaveCount(0);
  });

  test("the owner reviews the request, ticks a door and 15 minutes, and accepts", async ({ page }) => {
    const w = world({ open: requested() });
    await spliceIdentity(page, { switchOn: true, viewer: OWNER });
    await serveSupport(page, w);
    await openRun(page);
    await expect(page.getByText(SUPPORT_BANNER.ASKED(HELPER))).toBeVisible();
    await page.getByRole("button", { name: SUPPORT_BANNER.REVIEW }).click();
    const consent = page.getByTestId("support-consent");
    await expect(consent.getByText(SUPPORT_CONSENT.NOTHING_OPENS)).toBeVisible();
    await expect(consent.getByTestId("support-consent-copy")).toContainText(SUPPORT_CONSENT.REACH);
    await expect(consent.getByRole("button", { name: SUPPORT_CONSENT.ACCEPT })).toBeDisabled();
    await consent.getByRole("checkbox", { name: /Files/ }).click();
    await expect(consent.getByRole("button", { name: SUPPORT_CONSENT.ACCEPT })).toBeDisabled(); // a door, no time
    await consent.getByRole("radio", { name: SUPPORT_CONSENT.MINUTES(15) }).click();
    await consent.getByRole("button", { name: SUPPORT_CONSENT.ACCEPT }).click();
    const banner = page.getByTestId("support-active");
    await expect(banner.getByText(SUPPORT_BANNER.ACTIVE_OTHERS(HELPER))).toBeVisible();
    await expect(banner).toContainText("Files");
    await expect(banner).not.toContainText("Commands");
    expect(w.calls.find((c) => c.path.endsWith("/accept"))?.body).toEqual({ doors: ["files"], minutes: 15 });
  });

  test("the owner declines, and the banner is gone with no line for the owner", async ({ page }) => {
    const w = world({ open: requested() });
    await spliceIdentity(page, { switchOn: true, viewer: OWNER });
    await serveSupport(page, w);
    await openRun(page);
    await page.getByRole("button", { name: SUPPORT_BANNER.REVIEW }).click();
    await page.getByTestId("support-consent").getByRole("button", { name: SUPPORT_CONSENT.DECLINE }).click();
    await expect(page.getByTestId("support-requested")).toHaveCount(0);
    await expect(page.getByTestId("support-ended")).toHaveCount(0);
    expect(w.calls.some((c) => c.path.endsWith("/decline"))).toBe(true);
  });

  test("the owner's End now ends the help, and the ended line has Close", async ({ page }) => {
    const w = world({ open: { ...requested(), state: "active", doors: ["terminal"], minutes: 30, expires_at: new Date(Date.now() + 30 * 60_000).toISOString() } });
    await spliceIdentity(page, { switchOn: true, viewer: OWNER });
    await serveSupport(page, w);
    await openRun(page);
    await page.getByRole("button", { name: SUPPORT_BANNER.END_NOW }).click();
    const ended = page.getByTestId("support-ended");
    await expect(ended.getByText("Help ended: you ended it.")).toBeVisible();
    expect(w.calls.some((c) => c.path.endsWith("/end"))).toBe(true);
    await ended.getByRole("button", { name: SUPPORT_BANNER.CLOSE }).click();
    await expect(page.getByTestId("support-ended")).toHaveCount(0);
  });

  test("a helper sees the banner with You are helping and End now", async ({ page }) => {
    const w = world({ open: { ...requested(), state: "active", doors: ["terminal", "files"], minutes: 30, expires_at: new Date(Date.now() + 30 * 60_000).toISOString() } });
    await spliceIdentity(page, { switchOn: true, viewer: HELPER });
    await serveSupport(page, w);
    await openRun(page);
    const banner = page.getByTestId("support-active");
    await expect(banner.getByText(SUPPORT_BANNER.ACTIVE_HELPER)).toBeVisible();
    await expect(banner.getByRole("button", { name: SUPPORT_BANNER.END_NOW })).toBeVisible();
  });

  const helping = (): World => world({ open: { ...requested(), state: "active", doors: ["terminal"], minutes: 30, expires_at: new Date(Date.now() + 30 * 60_000).toISOString() } });

  test("enter: the helper's terminal opens on a ticket, read-only while the owner holds it, with no Take over and no hint", async ({ page }) => {
    await spliceIdentity(page, { switchOn: true, viewer: HELPER });
    await serveSupport(page, helping());
    const runId = await helpedTerminalRun(page);
    const { opens } = await stubAttachSocket(page, (_n, ws) => ws.send(attachModeFrame(true, { principal: OWNER })));
    await gotoConsole(page);
    await navToRoute(page, `/runs/${runId}`);
    const pane = page.getByTestId("run-terminal-pane");
    await expect(pane.locator(".xterm-screen").first()).toBeVisible();
    await expect(pane.getByText(RUN_COCKPIT.heldBy(OWNER))).toBeVisible();
    await expect(pane.getByText(RUN_COCKPIT.watchingReadOnly)).toBeVisible();
    await expect(pane.getByRole("button", { name: RUN_COCKPIT.takeOver })).toHaveCount(0);
    await expect(pane.getByText(RUN_COCKPIT.heldHint)).toHaveCount(0);
    expect(opens()).toBe(1);
  });

  test("cut: the owner joining closes the helper's terminal, which says so and offers Reconnect only", async ({ page }) => {
    await spliceIdentity(page, { switchOn: true, viewer: HELPER });
    await serveSupport(page, helping());
    const runId = await helpedTerminalRun(page);
    let firstWs: WebSocketRoute | null = null;
    const { opens } = await stubAttachSocket(page, (n, ws) => {
      if (n === 1) firstWs = ws;
      ws.send(attachModeFrame(false, { principal: HELPER }));
    });
    await gotoConsole(page);
    await navToRoute(page, `/runs/${runId}`);
    const pane = page.getByTestId("run-terminal-pane");
    await expect(pane.locator(".xterm-screen").first()).toBeVisible();
    await expect(pane.getByText(RUN_COCKPIT.driving)).toBeVisible();

    await firstWs!.close({ code: 1008, reason: "The run's owner joined" });
    const strip = pane.getByTestId("support-owner-joined");
    await expect(strip.getByText(SUPPORT_BANNER.OWNER_JOINED)).toBeVisible();
    await expect(strip.getByText(SUPPORT_BANNER.OWNER_DRIVING)).toBeVisible();
    await expect(pane.getByRole("button", { name: RUN_COCKPIT.takeOver })).toHaveCount(0);
    expect(opens()).toBe(1);

    await strip.getByRole("button", { name: SUPPORT_BANNER.RECONNECT }).click();
    await expect.poll(() => opens(), { timeout: 10_000, message: "Reconnect never opened a second socket" }).toBe(2);
    await expect(pane.getByTestId("support-owner-joined")).toHaveCount(0);
  });

  // The helper's doors on the run page (board M5c). The hermetic backend has no VM, no SSH gateway and no helper, so the
  // seeded RUNNING fixture 2 is spliced into the owner's interactive Linux VM run with two declared apps, /healthz
  // gains an SSH block, and the Files panel's rules are stubbed; the server half is pinned by the Go support tests.
  // The harness's bare admin bearer is an admin in the admin view, which is a helper's place.
  const doorsWorld = (doors: string[]): World =>
    world({ open: { ...requested(), state: "active", doors, minutes: 30, expires_at: new Date(Date.now() + 30 * 60_000).toISOString() } });

  async function helpedDoorsRun(page: Page): Promise<string> {
    const { id: runId } = await findRunningFixture(page);
    let cached: Record<string, unknown> | null = null;
    await page.route(`**/api/v1/runs/${runId}`, async (route) => {
      if (route.request().method() !== "GET") return route.fallback();
      if (!cached) {
        const response = await route.fetch();
        cached = {
          ...(await response.json()),
          interactive: true,
          state: "RUNNING",
          created_by: OWNER,
          operator_owned: false,
          machine_kind: "linux_vm",
          agent_exec_id: "exec-e2e",
          ui_apps: [
            { name: "code-server", port: 8080, path: "/" },
            { name: "preview", port: 3000, path: "/", inbound: true, callers: ["ci-bot"] },
          ],
        };
      }
      await route.fulfill({ json: cached! });
    });
    await page.route("**/healthz", async (route) => {
      const response = await route.fetch();
      const body = await response.json();
      await route.fulfill({ response, json: { ...body, ssh: { enabled: true, advertise_addr: "ssh.example.com:2222" } } });
    });
    await page.route(new RegExp(`/api/v1/runs/${runId}/desktop/files(\\?.*)?$`), (route) =>
      route.fulfill({ json: { folder: "~/Transfers", rules: { paste_in: "allowed", copy_out: "blocked", upload: "allowed", download: "blocked" }, files: [] } }),
    );
    await stubAttachTicket(page, runId);
    await stubAttachSocket(page, (_n, ws) => ws.send(attachModeFrame(false, { principal: HELPER })));
    return runId;
  }

  async function openHelpedRun(page: Page, w: World, viewer = HELPER): Promise<{ desktop: Locator; attach: Locator }> {
    await spliceIdentity(page, { switchOn: true, viewer });
    await serveSupport(page, w);
    const runId = await helpedDoorsRun(page);
    await gotoConsole(page);
    await navToRoute(page, `/runs/${runId}`);
    await expect(page.getByTestId("run-summary-header")).toBeVisible();
    return {
      desktop: page.getByRole("region", { name: DESKTOP.title }),
      attach: page.locator("section").filter({ has: page.getByText("Attach from your terminal", { exact: true }) }).last(),
    };
  }

  const closedLine = (...doors: Parameters<typeof SUPPORT_HELPER.doorsClosed>[0]) => SUPPORT_HELPER.doorsClosed(doors);
  const SSH_COMMAND = /^ssh .*@ssh\.example\.com -p 2222$/;

  test("doors: every door open draws Open desktop, the Files panel, SSH, Sync and the web apps, and none of the owner's controls", async ({ page }) => {
    const { desktop, attach } = await openHelpedRun(page, doorsWorld(["terminal", "commands", "files", "forward", "web_apps", "desktop"]));
    await desktop.scrollIntoViewIfNeeded();
    await expect(desktop.getByRole("button", { name: UI_APPS_LANE.cta("desktop") })).toBeVisible();
    await expect(desktop.getByRole("region", { name: DESKTOP_FILES.title })).toBeVisible();
    await expect(desktop.getByRole("button", { name: DESKTOP.pauseToSignIn })).toHaveCount(0);
    await attach.scrollIntoViewIfNeeded();
    await expect(attach.getByText(SSH_COMMAND)).toBeVisible();
    await expect(attach.getByText(RUN_SYNC.LANE_TITLE)).toBeVisible();
    await expect(attach.getByRole("button", { name: UI_APPS_LANE.cta("code-server") })).toBeVisible();
    await expect(attach.getByRole("button", { name: UI_APPS_LANE.cta("preview") })).toBeVisible();
    await expect(attach.getByText(INBOUND_APP.TAG)).toBeVisible();
    await expect(attach.getByText(RUN_CLI.TITLE)).toHaveCount(0);
    await expect(attach.getByText("Manage SSH keys")).toHaveCount(0);
    await expect(attach.getByText(INBOUND_APP.ONLY_YOU)).toHaveCount(0);
    await expect(attach.getByText(INBOUND_APP.COPY_LINK)).toHaveCount(0);
    await expect(attach.getByText(INBOUND_APP.COPY_ADDRESS)).toHaveCount(0);
    await expect(page.getByTestId("helper-doors-closed")).toHaveCount(0);
  });

  test("doors: terminal and commands only keeps SSH, names what is closed, and draws no Desktop tile", async ({ page }) => {
    const { desktop, attach } = await openHelpedRun(page, doorsWorld(["terminal", "commands"]));
    await attach.scrollIntoViewIfNeeded();
    await expect(attach.getByText(SSH_COMMAND)).toBeVisible();
    await expect(attach.getByText(closedLine("files", "forward"))).toBeVisible();
    await expect(attach.getByText(RUN_SYNC.LANE_TITLE)).toBeVisible();
    await expect(attach.getByText(closedLine("files"), { exact: true })).toBeVisible();
    await expect(attach.getByText(closedLine("web_apps"))).toBeVisible();
    await expect(attach.getByRole("button", { name: UI_APPS_LANE.cta("code-server") })).toHaveCount(0);
    await expect(desktop).toHaveCount(0);
  });

  test("doors: files and web apps keeps the Desktop tile for its Files panel and says P-01 in the terminal pane", async ({ page }) => {
    const { desktop, attach } = await openHelpedRun(page, doorsWorld(["files", "web_apps"]));
    await desktop.scrollIntoViewIfNeeded();
    await expect(desktop.getByText(closedLine("desktop"))).toBeVisible();
    await expect(desktop.getByRole("button", { name: UI_APPS_LANE.cta("desktop") })).toHaveCount(0);
    await expect(desktop.getByRole("region", { name: DESKTOP_FILES.title })).toBeVisible();
    await attach.scrollIntoViewIfNeeded();
    await expect(attach.getByText(closedLine("terminal", "commands", "forward"))).toBeVisible();
    await expect(attach.getByRole("button", { name: UI_APPS_LANE.cta("code-server") })).toBeVisible();
    await expect(page.getByTestId("run-terminal-pane").getByText(closedLine("terminal"))).toBeVisible();
  });

  test("doors: web apps only closes SSH and Sync with P-01 and draws no Desktop tile", async ({ page }) => {
    const { desktop, attach } = await openHelpedRun(page, doorsWorld(["web_apps"]));
    await attach.scrollIntoViewIfNeeded();
    await expect(attach.getByText(closedLine("terminal", "commands", "files", "forward"))).toBeVisible();
    await expect(attach.getByText(closedLine("files"), { exact: true })).toBeVisible();
    await expect(attach.getByText(SSH_COMMAND)).toHaveCount(0);
    await expect(attach.getByRole("button", { name: UI_APPS_LANE.cta("preview") })).toBeVisible();
    await expect(desktop).toHaveCount(0);
  });

  test("doors: a click the server answers support_grant_ended reads P-02 on Open desktop", async ({ page }) => {
    const { desktop } = await openHelpedRun(page, doorsWorld(["desktop", "files", "web_apps"]));
    await page.route("**/api/v1/runs/*/attach/ticket", (route) =>
      route.fulfill({ status: 403, json: { error: "the support grant on this run ended", reason: "support_grant_ended" } }),
    );
    await desktop.scrollIntoViewIfNeeded();
    await desktop.getByRole("button", { name: UI_APPS_LANE.cta("desktop") }).click();
    await expect(desktop.getByText(SUPPORT_HELPER.ended)).toBeVisible();
  });

  test("doors: an SSO super admin with no open grant sees neither tile, only Request help", async ({ page }) => {
    const none = await openHelpedRun(page, world());
    await expect(page.getByTestId("run-summary-header")).toBeVisible();
    await expect(none.desktop).toHaveCount(0);
    await expect(none.attach).toHaveCount(0);
    await expect(page.getByRole("button", { name: SUPPORT_REQUEST.TITLE })).toBeVisible();
  });
});
