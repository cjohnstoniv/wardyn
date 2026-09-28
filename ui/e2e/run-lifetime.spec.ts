/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// RL-15 (#580, #1197 L5) — long-holds design rev 4 §2.3/§3/§4.1, the mock
// "Before and at the end" / "Paused, and lost then revived": the run page's
// Ends row, the 24h/1h/10m warning, ended/lost/revive and pausing.
//
// The Ends row itself is proven against the REAL backend (a seeded run, a
// real PATCH /runs/{id} round trip) — no exotic state needed. Lost, ended
// and paused are server facts this harness cannot produce for real (no
// reboot, no control-plane outage, no idle sandbox to freeze), so those
// splice the fields onto a real run's GET response — attach-stub.ts's own
// precedent — and mock the action endpoint each banner calls, to prove the
// UI sends the right request and redraws from what comes back.
import { randomUUID } from "node:crypto";
import type { Page } from "@playwright/test";
import { test, expect, consoleAPI, gotoConsole, navToRoute } from "./fixtures";

async function seedRun(page: Page, task: string): Promise<string> {
  const created = await consoleAPI(page, "POST", "/api/v1/runs", { agent: "claude-code", task });
  expect(created.status, created.text).toBe(201);
  return JSON.parse(created.text).id as string;
}

// Splice fields onto the run's REAL GET /runs/{id} response — every other
// field (repo, created_by, task, state) still comes from the real backend.
// Cached after the first read (attach-stub.ts's own reasoning): the page
// polls this route, and a stale in-flight route.fetch() at teardown would
// otherwise throw "Response has been disposed".
async function stubRunDetail(page: Page, runId: string, overlay: Record<string, unknown>): Promise<void> {
  let cached: Record<string, unknown> | null = null;
  await page.route(`**/api/v1/runs/${runId}`, async (route) => {
    if (route.request().method() !== "GET") return route.fallback();
    if (!cached) {
      const json = await (await route.fetch()).json();
      cached = { ...json, ...overlay };
    }
    await route.fulfill({ json: cached! });
  });
}

async function stubApprovals(page: Page, runId: string, approvals: unknown[]): Promise<void> {
  await page.route("**/api/v1/approvals*", (route) => {
    const url = new URL(route.request().url());
    if (url.searchParams.get("run_id") !== runId) return route.fallback();
    return route.fulfill({ json: approvals });
  });
}

test.describe("Run lifetime — the Ends row (real backend round trip)", () => {
  test("a fresh run has no end; 'Set an end…' sets one, and Extend pushes it further out", async ({ page }) => {
    await gotoConsole(page);
    const id = await seedRun(page, "run-lifetime e2e ends " + randomUUID().slice(0, 8));
    await navToRoute(page, `/runs/${id}`);

    const row = page.getByTestId("run-ends-row");
    await expect(row.getByText("No end")).toBeVisible();

    await row.getByRole("button", { name: "Set an end…" }).click();
    const dialog = page.getByRole("dialog");
    await expect(dialog).toBeVisible();
    const input = dialog.locator('input[type="datetime-local"]');
    const target = new Date(Date.now() + 3 * 24 * 3600_000);
    const pad = (n: number) => String(n).padStart(2, "0");
    const value = `${target.getFullYear()}-${pad(target.getMonth() + 1)}-${pad(target.getDate())}T${pad(target.getHours())}:${pad(target.getMinutes())}`;
    await input.fill(value);
    await dialog.getByRole("button", { name: "Save" }).click();

    await expect(row.getByText(/^Ends /)).toBeVisible();
    const afterSet = await consoleAPI(page, "GET", `/api/v1/runs/${id}`);
    const setEndsAt = JSON.parse(afterSet.text).ends_at as string;
    expect(setEndsAt).toBeTruthy();

    // Extend "1 more day" moves the CAPTURED end forward, not "now" — proven
    // at the unit level (run-ends-row.test.tsx); here it's the real PATCH.
    await row.getByRole("button", { name: "Extend" }).click();
    await page.getByText("1 more day").click();
    await expect(row.getByText(/^Ends /)).toBeVisible();
    const afterExtend = await consoleAPI(page, "GET", `/api/v1/runs/${id}`);
    const extendedEndsAt = JSON.parse(afterExtend.text).ends_at as string;
    expect(Date.parse(extendedEndsAt)).toBeGreaterThan(Date.parse(setEndsAt));
    expect(Math.abs(Date.parse(extendedEndsAt) - Date.parse(setEndsAt) - 24 * 3600_000)).toBeLessThan(60_000);
  });
});

test.describe("Run lifetime — lost and revive", () => {
  test("a run lost to a reboot: the danger banner, Revive posts and the page redraws lost-free", async ({ page }) => {
    await gotoConsole(page);
    const id = await seedRun(page, "run-lifetime e2e lost-reboot " + randomUUID().slice(0, 8));
    await stubRunDetail(page, id, { lost_reason: "reboot", agent: "claude-code" });

    let revived = false;
    await page.route(`**/api/v1/runs/${id}/revive`, async (route) => {
      revived = true;
      await route.fulfill({ json: { run_id: id, denied_added: ["evil.example"], proxy_release: "r1" } });
    });

    await navToRoute(page, `/runs/${id}`);
    const banner = page.getByTestId("run-lifetime-lost");
    await expect(banner).toBeVisible();
    await expect(banner.getByText("This run's sandbox stopped")).toBeVisible();
    await expect(banner.getByText(/continues the Claude Code conversation/)).toBeVisible();

    await banner.getByRole("button", { name: "Revive" }).click();
    await expect.poll(() => revived).toBe(true);
    await expect(page.getByText("Policy updated at revive: 1 host now blocked.")).toBeVisible();
  });

  test("a run lost to an outage: the outage sentence joins the lost body", async ({ page }) => {
    await gotoConsole(page);
    const id = await seedRun(page, "run-lifetime e2e lost-outage " + randomUUID().slice(0, 8));
    await stubRunDetail(page, id, { lost_reason: "outage", agent: "codex-cli" });
    await navToRoute(page, `/runs/${id}`);

    const banner = page.getByTestId("run-lifetime-lost");
    await expect(banner.getByText(/starts a new session/)).toBeVisible();
    await expect(banner.getByText(/unreachable for over an hour/)).toBeVisible();
  });
});

test.describe("Run lifetime — ended (the lease ran out)", () => {
  test("Extend and revive PATCHes a future end, then POSTs revive", async ({ page }) => {
    await gotoConsole(page);
    const id = await seedRun(page, "run-lifetime e2e ended " + randomUUID().slice(0, 8));
    await stubRunDetail(page, id, {
      lost_reason: "ended",
      lost_at: new Date(Date.now() - 3600_000).toISOString(),
    });

    let patchedEndsAt = "";
    await page.route(`**/api/v1/runs/${id}`, async (route) => {
      if (route.request().method() !== "PATCH") return route.fallback();
      patchedEndsAt = JSON.parse(route.request().postData() ?? "{}").ends_at;
      await route.fulfill({ json: { id, ends_at: patchedEndsAt, wait_budget_sec: 0, capped: [] } });
    });
    let revived = false;
    await page.route(`**/api/v1/runs/${id}/revive`, async (route) => {
      revived = true;
      await route.fulfill({ json: { run_id: id, denied_added: [], proxy_release: "r1" } });
    });

    await navToRoute(page, `/runs/${id}`);
    const banner = page.getByTestId("run-lifetime-ended");
    await expect(banner.getByText("This run ended at its end time")).toBeVisible();

    await banner.getByRole("button", { name: "Extend and revive" }).click();
    await expect.poll(() => revived).toBe(true);
    expect(Date.parse(patchedEndsAt)).toBeGreaterThan(Date.now());
  });
});

test.describe("Run lifetime — paused", () => {
  test("waiting on a decision: reads the open request's own expiry, Resume now posts /resume", async ({ page }) => {
    await gotoConsole(page);
    const id = await seedRun(page, "run-lifetime e2e paused-waiting " + randomUUID().slice(0, 8));
    const until = new Date(Date.now() + 3600_000).toISOString();
    await stubRunDetail(page, id, { paused_at: new Date(Date.now() - 60_000).toISOString(), paused_reason: "waiting" });
    await stubApprovals(page, id, [
      {
        id: "appr-1",
        run_id: id,
        kind: "egress_domain",
        requested_scope: { host: "example.com" },
        state: "PENDING",
        requested_at: new Date().toISOString(),
        expires_at: until,
      },
    ]);
    let resumed = false;
    await page.route(`**/api/v1/runs/${id}/resume`, async (route) => {
      resumed = true;
      await route.fulfill({ json: { id, paused: false } });
    });

    await navToRoute(page, `/runs/${id}`);
    const banner = page.getByTestId("run-lifetime-paused");
    await expect(banner.getByText("Paused while waiting for approval")).toBeVisible();
    await expect(banner.getByText(/The request stays open until/)).toBeVisible();

    await banner.getByRole("button", { name: "Resume now" }).click();
    await expect.poll(() => resumed).toBe(true);
  });

  test("idle: the minutes come from the run's own captured pause_idle_after_sec", async ({ page }) => {
    await gotoConsole(page);
    const id = await seedRun(page, "run-lifetime e2e paused-idle " + randomUUID().slice(0, 8));
    await stubRunDetail(page, id, {
      paused_at: new Date(Date.now() - 60_000).toISOString(),
      paused_reason: "idle",
      run_limits: { pause_idle_after_sec: 900 },
    });
    await navToRoute(page, `/runs/${id}`);
    await expect(page.getByText("Paused — nobody's been here for 15 minutes")).toBeVisible();
  });
});

test.describe("Run lifetime — the ending-soon warning", () => {
  test("a run 9 minutes from its end: the warning banner, dismissible", async ({ page }) => {
    await gotoConsole(page);
    const id = await seedRun(page, "run-lifetime e2e warning " + randomUUID().slice(0, 8));
    await stubRunDetail(page, id, { ends_at: new Date(Date.now() + 9 * 60_000).toISOString() });
    await navToRoute(page, `/runs/${id}`);

    const banner = page.getByTestId("run-lifetime-warning");
    await expect(banner.getByText("This run ends in 10 minutes")).toBeVisible();
    await banner.getByRole("button", { name: "Dismiss" }).click();
    await expect(banner).toHaveCount(0);
  });
});
