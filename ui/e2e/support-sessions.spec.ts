/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { Page } from "@playwright/test";
import { test, expect, gotoConsole, navTo } from "./fixtures";
import { SUPPORT_BANNER, SUPPORT_CONSENT, SUPPORT_REQUEST } from "../src/app/components/wardyn/copy/support";

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

  test("End now ends the help, for the owner and the helper, and the ended line has Close", async ({ page }) => {
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
});
