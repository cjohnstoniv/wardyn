/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// WARDYN_BASE_PATH (#1154): the console behind a reverse proxy at a sub-path.
// Run it in the harness's base-path mode —
//   WARDYN_E2E_BASE_PATH=/wardyn scripts/run-ui-e2e.sh base-path
// — where the backend serves under /wardyn behind test/basepathproxy and the
// base URL is the proxy's http://localhost:<port>/wardyn/. Every path below is
// RELATIVE to that base URL, so the same spec also passes at the root (the
// default mode, where BASE is ""), which is the byte-identical-default half of
// the contract. The proxy-only assertions run only in base-path mode.
import type { Page, Request } from "@playwright/test";
import { test, expect, sidebarLink, ADMIN_TOKEN } from "./fixtures";
import { attachModeFrame, stubAttachSocket, stubAttachTicket, stubInteractiveRun } from "./attach-stub";

const BASE = process.env.WARDYN_E2E_BASE_PATH ?? "";
const auth = { Authorization: `Bearer ${ADMIN_TOKEN}` };

/** Every request the page makes to the console's own origin, by path. */
function trackPaths(page: Page, baseURL: string): string[] {
  const origin = new URL(baseURL).origin;
  const paths: string[] = [];
  page.on("request", (r: Request) => {
    const u = new URL(r.url());
    if (u.origin === origin) paths.push(u.pathname);
  });
  return paths;
}

async function runningFixtureId(page: Page): Promise<string> {
  const runs = (await (await page.request.get("api/v1/runs?limit=1000", { headers: auth })).json()) as { id: string; task: string }[];
  const run = runs.find((r) => r.task === "e2e fixture 2");
  if (!run) throw new Error("seeded fixture 2 not found");
  return run.id;
}

test.describe("served under WARDYN_BASE_PATH", () => {
  test("the console loads under the base, and every request it makes stays there", async ({ page, baseURL }) => {
    const paths = trackPaths(page, baseURL!);
    // The shell and its assets, not the API: a console read may 404 by design.
    const failed: string[] = [];
    page.on("response", (r) => {
      const path = new URL(r.url()).pathname;
      if (r.status() >= 400 && !path.includes("/api/v1/")) failed.push(`${r.status()} ${path}`);
    });
    await page.goto("./");
    await expect(sidebarLink(page, "Runs")).toBeVisible();
    expect(new URL(page.url()).pathname.startsWith(`${BASE}/`)).toBe(true);
    // The shell, the hashed assets, /healthz and the API: all under the base.
    expect(paths.filter((p) => p.startsWith(`${BASE}/assets/`)).length).toBeGreaterThan(0);
    expect(paths.filter((p) => p.startsWith(`${BASE}/api/v1/`)).length).toBeGreaterThan(0);
    expect(paths).toContain(`${BASE}/healthz`);
    expect(paths.filter((p) => !p.startsWith(`${BASE}/`))).toEqual([]);
    expect(failed).toEqual([]);
  });

  test("a deep link survives a refresh", async ({ page }) => {
    const id = await runningFixtureId(page);
    await page.goto(`runs/${id}`);
    const heading = page.getByRole("heading", { name: "e2e fixture 2", level: 1 });
    await expect(heading).toBeVisible();
    await page.reload();
    await expect(heading).toBeVisible();
    expect(new URL(page.url()).pathname).toBe(`${BASE}/runs/${id}`);
    // A console route reached by clicking (the router's basename) stays under it.
    await sidebarLink(page, "Approvals").click();
    await expect(page).toHaveURL((u) => u.pathname.startsWith(`${BASE}/`) && u.pathname.endsWith("/approvals"));
  });

  test("an API call answers under the base, and the host root is not Wardyn's", async ({ page }) => {
    const me = await page.request.get("api/v1/me", { headers: auth });
    expect(me.status()).toBe(200);
    expect(await me.json()).toHaveProperty("role");
    const health = await page.request.get("healthz");
    expect(health.status()).toBe(200);
    test.skip(!BASE, "the root half needs the base-path mode's proxy");
    for (const rootPath of ["/api/v1/me", "/healthz", "/"]) {
      const res = await page.request.get(rootPath, { headers: auth });
      expect(res.status(), rootPath).toBe(404);
      expect(await res.text(), rootPath).toContain("neighbouring application");
    }
  });

  test("the terminal attaches on a WebSocket under the base", async ({ page }) => {
    const id = await runningFixtureId(page);
    await stubInteractiveRun(page, id);
    await stubAttachTicket(page, id);
    const sockets: string[] = [];
    await stubAttachSocket(page, (_n, ws) => {
      sockets.push(new URL(ws.url()).pathname);
      ws.send(attachModeFrame(false));
    });
    await page.goto(`runs/${id}`);
    await expect(page.getByTestId("run-terminal-pane").locator(".xterm-screen").first()).toBeVisible();
    await expect.poll(() => sockets.length).toBeGreaterThan(0);
    expect(sockets[0]).toBe(`${BASE}/api/v1/runs/${id}/attach`);
  });

  test("sign-out posts under the base and lands on the sign-in screen there", async ({ page }) => {
    await page.goto("./");
    await expect(sidebarLink(page, "Runs")).toBeVisible();
    const logout = page.waitForRequest((r) => r.method() === "POST" && new URL(r.url()).pathname.endsWith("/api/v1/auth/logout"));
    await page.locator('header button[aria-haspopup="menu"]').click();
    await page.getByRole("menuitem", { name: "Sign out" }).click();
    expect(new URL((await logout).url()).pathname).toBe(`${BASE}/api/v1/auth/logout`);
    await expect(page.locator("#token")).toBeVisible();
    expect(new URL(page.url()).pathname.startsWith(`${BASE}/`)).toBe(true);
  });
});
