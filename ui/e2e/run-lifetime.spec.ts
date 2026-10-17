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
    // The row already read "Ends …" before the click, so wait on the stored end
    // itself: a read that lands before the PATCH would still see the old one.
    let extendedEndsAt = setEndsAt;
    await expect
      .poll(async () => {
        const res = await consoleAPI(page, "GET", `/api/v1/runs/${id}`);
        extendedEndsAt = JSON.parse(res.text).ends_at as string;
        return Date.parse(extendedEndsAt);
      })
      .toBeGreaterThan(Date.parse(setEndsAt));
    expect(Math.abs(Date.parse(extendedEndsAt) - Date.parse(setEndsAt) - 24 * 3600_000)).toBeLessThan(60_000);
  });
});

test.describe("Run lifetime — lost and revive", () => {
  // Overlay via a MUTABLE splice this test itself controls (not stubRunDetail's
  // cache-once helper), so Revive's success can be followed by a real redraw:
  // the same run, now clean, is what the very next poll picks up.
  async function stubMutableRunDetail(page: Page, runId: string, initialOverlay: Record<string, unknown>) {
    let overlay = initialOverlay;
    let base: Record<string, unknown> | null = null;
    await page.route(`**/api/v1/runs/${runId}`, async (route) => {
      if (route.request().method() !== "GET") return route.fallback();
      if (!base) base = await (await route.fetch()).json();
      await route.fulfill({ json: { ...base, ...overlay } });
    });
    return { setOverlay: (next: Record<string, unknown>) => { overlay = next; } };
  }

  test("a run lost to a reboot: the danger banner, Revive posts, and the page redraws lost-free", async ({ page }) => {
    await gotoConsole(page);
    const id = await seedRun(page, "run-lifetime e2e lost-reboot " + randomUUID().slice(0, 8));
    const detail = await stubMutableRunDetail(page, id, { lost_reason: "reboot", agent: "claude-code" });

    let revived = false;
    await page.route(`**/api/v1/runs/${id}/revive`, async (route) => {
      revived = true;
      detail.setOverlay({}); // a real revive clears lost_at/lost_reason server-side
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
    // The redraw: onChanged's refetch (not the 4s poll) picks up the cleared
    // overlay immediately, so the lost banner is gone without a page reload.
    await expect(banner).toHaveCount(0);
  });

  test("a run lost to an outage: F5, ONLY the outage sentence — never the reboot body or a harness-continuity line", async ({ page }) => {
    await gotoConsole(page);
    const id = await seedRun(page, "run-lifetime e2e lost-outage " + randomUUID().slice(0, 8));
    await stubRunDetail(page, id, { lost_reason: "outage", agent: "codex-cli", ends_at: new Date(Date.now() + 3600_000).toISOString() });
    await navToRoute(page, `/runs/${id}`);

    const banner = page.getByTestId("run-lifetime-lost");
    await expect(banner.getByText(/unreachable for over an hour/)).toBeVisible();
    await expect(banner.getByText(/machine it ran on restarted/)).toHaveCount(0);
    await expect(banner.getByText(/starts a new session/)).toHaveCount(0);
    await expect(banner.getByText(/continues the Claude Code conversation/)).toHaveCount(0);
  });
});

test.describe("Run lifetime — ended (the lease ran out)", () => {
  test("Extend and revive PATCHes a future end, then POSTs revive", async ({ page }) => {
    await gotoConsole(page);
    const id = await seedRun(page, "run-lifetime e2e ended " + randomUUID().slice(0, 8));
    await stubRunDetail(page, id, {
      lost_reason: "ended",
      lost_at: new Date(Date.now() - 3600_000).toISOString(),
      // F9 (PR #1317 review): Extend-and-revive is offered only for an
      // interactive run — a task run's agent cannot be started again
      // (reviveEligible, run_revive.go), and gets a plain Extend instead.
      // This case is deliberately the interactive one.
      interactive: true,
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

  // R2-2 (PR #1317 round-2 review, F9 REJECTED): extending an ended task run
  // changes nothing a person can observe (run_revive.go, run_end_wait.go) —
  // End run is the only real action, and the body drops the "Extend to
  // revive it" claim, which is false for a run that can never be revived.
  test("R2-2: a task (non-interactive) run offers End run ONLY — no Extend, no revive sentence", async ({ page }) => {
    await gotoConsole(page);
    const id = await seedRun(page, "run-lifetime e2e ended-task " + randomUUID().slice(0, 8));
    await stubRunDetail(page, id, {
      lost_reason: "ended",
      lost_at: new Date(Date.now() - 3600_000).toISOString(),
      interactive: false,
    });

    await navToRoute(page, `/runs/${id}`);
    const banner = page.getByTestId("run-lifetime-ended");
    await expect(banner.getByText("It has no network.")).toBeVisible();
    await expect(banner.getByText(/Extend to revive it/)).toHaveCount(0);
    await expect(banner.getByRole("button", { name: "Extend and revive" })).toHaveCount(0);
    await expect(banner.getByRole("button", { name: "Extend" })).toHaveCount(0);
    await expect(banner.getByRole("button", { name: "End run" })).toBeVisible();
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
