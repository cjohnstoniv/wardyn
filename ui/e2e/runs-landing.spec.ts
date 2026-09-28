/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #1197 L3 — the Runs landing page (design.md, D2). One test per state in
// design.md §6's L3 row: loading, error then Retry, first run (member), first
// run (Admin view), all quiet, populated with every row kind, "Earlier this
// week" expands, the ageing note + Include killed, no match then Clear,
// 400px with no horizontal scroll, and light/dark renders.
//
// The 9-fixture seeded backend (runs.spec.ts) proves the real GET /runs wire
// end to end; this file needs precise attention/ended_at combinations no
// seed script produces, so every scenario here fully replaces the GET /runs
// response via page.route — same technique runs.spec.ts's own splices use,
// just for the list endpoint instead of the detail one.
import { test, expect, asRealMember, gotoConsole, sidebarLink } from "./fixtures";
import type { Page } from "@playwright/test";

const RUNS_LIST_RE = /\/api\/v1\/runs(\?.*)?$/;

function baseRun(id: string, task: string, over: Record<string, unknown> = {}): Record<string, unknown> {
  const now = new Date().toISOString();
  return {
    id,
    task,
    title: task,
    created_at: now,
    updated_at: now,
    created_by: "admin-token",
    agent: "claude-code",
    repo: "acme/widgets",
    confinement_class: "CC2",
    state: "RUNNING",
    spiffe_id: `spiffe://${id}`,
    runner_target: "docker",
    ...over,
  };
}

type RunsListHandler = (url: URL) => {
  runs: unknown[];
  hiddenOlder?: number;
  hiddenKilled?: number;
  truncated?: boolean;
  delayMs?: number;
  status?: number;
};

async function mockRunsList(page: Page, handler: RunsListHandler): Promise<void> {
  await page.route(RUNS_LIST_RE, async (route) => {
    const url = new URL(route.request().url());
    // core.ts's probeAuth fires GET /api/v1/runs?limit=1 on every cold
    // document load — the SAME endpoint, but a cross-cutting shell read, not
    // RunsScreen's own. Handler scenarios below are written for the page's
    // own request (which always carries ?view=, api/runs.ts's
    // listRunsFiltered); let the probe's bare ?limit=1 hit the real backend
    // instead of an error/empty scenario, or the whole shell reads as
    // signed-out (a 500/empty answer there makes App.tsx's probeAuth verdict
    // "unreachable"/no-runs, not just this screen's own state).
    if (!url.searchParams.has("view")) return route.fallback();
    const { runs, hiddenOlder = 0, hiddenKilled = 0, truncated = false, delayMs, status = 200 } = handler(url);
    if (delayMs) await new Promise((r) => setTimeout(r, delayMs));
    await route.fulfill({
      status,
      contentType: "application/json",
      headers: {
        "X-Wardyn-Hidden-Older": String(hiddenOlder),
        "X-Wardyn-Hidden-Killed": String(hiddenKilled),
        ...(truncated ? { "X-Wardyn-Truncated": "true" } : {}),
      },
      body: JSON.stringify(status === 200 ? runs : { error: "boom" }),
    });
  });
}

async function mockHasRuns(page: Page, hasRuns: boolean): Promise<void> {
  // CACHE-AND-SERVE, not route.fetch()+refulfill per match — the same reason
  // fixtures.ts's mockMemberSetupStatus does it: the landing redirect, the
  // shell's poll and a screen's own mount all hit this endpoint, and a real
  // round trip PER match races Playwright disposing an in-flight route's
  // response ("apiResponse.json: Response has been disposed"). One real fetch,
  // then every match is fulfilled from the cached body. The glob keeps the
  // trailing `*`: the console re-reads with `?recheck=1`.
  let cached: Record<string, unknown> | null = null;
  await page.route("**/api/v1/setup/status*", async (route) => {
    if (!cached) {
      const body = (await (await route.fetch()).json()) as Record<string, unknown>;
      body.has_runs = hasRuns;
      cached = body;
    }
    // TS cannot narrow a `let` captured across the await above; the `if` does.
    await route.fulfill({ json: cached! });
  });
}

test.describe("Runs landing — page states (design.md §6 L3 row)", () => {
  test("loading: an aria-busy skeleton renders before the fetch settles", async ({ page }) => {
    await mockRunsList(page, () => ({ runs: [], delayMs: 500 }));
    await gotoConsole(page);
    await expect(page.getByLabel("Loading runs")).toBeVisible();
    await expect(page.getByLabel("Loading runs")).toHaveAttribute("aria-busy", "true");
  });

  test("error, then Retry recovers", async ({ page }) => {
    let fail = true;
    await mockRunsList(page, () => (fail ? { runs: [], status: 500 } : { runs: [baseRun("r1", "Recovered run")] }));
    await gotoConsole(page);
    await expect(page.getByText("Wardyn isn't answering. Try again.")).toBeVisible();
    fail = false;
    await page.getByRole("button", { name: "Retry" }).click();
    await expect(page.getByText("Recovered run")).toBeVisible();
  });

  test("first run, User view (member): the composer plus the member empty state", async ({ page }) => {
    await mockRunsList(page, () => ({ runs: [] }));
    await asRealMember(page);
    await mockHasRuns(page, false);
    // Direct to /runs, not "/" — a fresh, never-dismissed member with
    // has_runs=false legitimately lands on the guided tour from "/"
    // (App.tsx's FirstRunLanding, unrelated to this page); this test is
    // about RunsScreen's OWN first-run rendering once it is the page open.
    await page.goto("/runs");
    await expect(sidebarLink(page, "Runs")).toBeVisible();
    await expect(page.getByText("Runs you launch appear here")).toBeVisible();
    await expect(page.getByRole("form", { name: "Start a run" })).toBeVisible();
  });

  test("first run, Admin view: the inline empty state, no composer", async ({ page }) => {
    await mockRunsList(page, () => ({ runs: [] }));
    await mockHasRuns(page, false);
    await gotoConsole(page, "admin");
    await expect(page.getByText("No runs yet")).toBeVisible();
    await expect(page.getByRole("form", { name: "Start a run" })).toHaveCount(0);
  });

  test("all quiet: nothing needs you and nothing is running, Ended sections still show", async ({ page }) => {
    await mockRunsList(page, () => ({
      runs: [baseRun("e1", "Finished earlier", { state: "COMPLETED", ended_at: new Date().toISOString() })],
    }));
    await gotoConsole(page);
    await expect(page.getByText("Nothing needs you and nothing is running.")).toBeVisible();
    await expect(page.getByText("Finished earlier")).toBeVisible();
  });

  test("populated: every row kind renders its own glyph hue and status word", async ({ page }) => {
    await mockRunsList(page, () => ({
      runs: [
        baseRun("a1", "Held approval", { state: "RUNNING", attention: { kind: "approval", by: "you", pending: 1 } }),
        baseRun("a2", "AWS sign-in wait", { state: "RUNNING", attention: { kind: "reauth", by: "you", pending: 1 } }),
        baseRun("a3", "Azure DevOps sign-in wait", {
          state: "RUNNING",
          attention: { kind: "ado_consent", by: "you", pending: 1 },
        }),
        baseRun("a4", "Lost sandbox", { state: "RUNNING", attention: { kind: "lost", by: "you", pending: 0 } }),
        // H-3: a run needing an ADMIN (by="admin") is NOT in Needs you — it
        // sits under Running, amber, "Waiting for an admin".
        baseRun("a5", "Needs an admin", {
          state: "RUNNING",
          attention: { kind: "approval", by: "admin", pending: 1 },
        }),
        baseRun("r1", "Live and running", { state: "RUNNING" }),
        baseRun("r2", "Starting up", { state: "STARTING" }),
        baseRun("r3", "Queued run", { state: "PENDING" }),
        baseRun("e1", "Wrapped up fine", { state: "COMPLETED", ended_at: new Date().toISOString() }),
        baseRun("e2", "Blew up", { state: "FAILED", ended_at: new Date().toISOString() }),
        baseRun("e3", "Stopped by hand", { state: "STOPPED", ended_at: new Date().toISOString() }),
        baseRun("e4", "Killed on purpose", { state: "KILLED", ended_at: new Date().toISOString() }),
        baseRun("e5", "Archived long ago", { state: "ARCHIVED", ended_at: new Date().toISOString() }),
      ],
    }));
    await gotoConsole(page);

    await expect(page.getByRole("heading", { name: /Needs you/ })).toBeVisible();
    await expect(page.getByText("Held approval")).toBeVisible();
    await expect(page.getByText("Needs your approval")).toBeVisible();
    // Both "a1" (by=you) and "a5" (H-3's by=admin) carry this same subword —
    // the design table doesn't differ on it, only on the WORD above it.
    await expect(page.getByText("1 waiting · sandbox held").first()).toBeVisible();
    await expect(page.getByText("AWS sign-in wait")).toBeVisible();
    await expect(page.getByText("Waiting for your AWS sign-in")).toBeVisible();
    await expect(page.getByText("Azure DevOps sign-in wait")).toBeVisible();
    await expect(page.getByText("Waiting for your Azure DevOps sign-in")).toBeVisible();
    await expect(page.getByText("Lost sandbox")).toBeVisible();
    await expect(page.getByText("Sandbox stopped")).toBeVisible();

    await expect(page.getByRole("heading", { name: "Running" })).toBeVisible();
    await expect(page.getByText("Needs an admin")).toBeVisible();
    await expect(page.getByText("Waiting for an admin")).toBeVisible();
    await expect(page.getByText("Live and running")).toBeVisible();
    await expect(page.getByText("Starting up")).toBeVisible();
    await expect(page.getByText("Starting", { exact: true })).toBeVisible();
    await expect(page.getByText("Queued run")).toBeVisible();
    await expect(page.getByText("Queued", { exact: true })).toBeVisible();

    await expect(page.getByRole("heading", { name: "Ended today" })).toBeVisible();
    await expect(page.getByText("Wrapped up fine")).toBeVisible();
    await expect(page.getByText("Completed", { exact: true })).toBeVisible();
    await expect(page.getByText("Blew up")).toBeVisible();
    await expect(page.getByText("Failed", { exact: true })).toBeVisible();
    await expect(page.getByText("Stopped by hand")).toBeVisible();
    await expect(page.getByText("Stopped", { exact: true })).toBeVisible();
    await expect(page.getByText("Killed on purpose")).toBeVisible();
    await expect(page.getByText("Killed", { exact: true })).toBeVisible();
    await expect(page.getByText("Archived long ago")).toBeVisible();
    await expect(page.getByText("Archived", { exact: true })).toBeVisible();
  });

  test("'Earlier this week' expands, aria-expanded toggles, and focus stays on the toggle", async ({ page }) => {
    await mockRunsList(page, () => ({
      runs: [
        baseRun("e1", "From three days ago", {
          state: "COMPLETED",
          ended_at: new Date(Date.now() - 3 * 24 * 3600_000).toISOString(),
        }),
      ],
    }));
    await gotoConsole(page);

    const toggle = page.getByRole("button", { name: /Earlier this week/ });
    await expect(toggle).toHaveAttribute("aria-expanded", "false");
    await expect(page.getByText("From three days ago")).toHaveCount(0);

    await toggle.click();
    await expect(toggle).toHaveAttribute("aria-expanded", "true");
    await expect(page.getByText("From three days ago")).toBeVisible();
    await expect(toggle).toBeFocused();
  });

  test("the ageing note hides a 2-day-old killed run, and Include killed shows it", async ({ page }) => {
    const killedRun = baseRun("k1", "Killed 2 days ago", {
      state: "KILLED",
      ended_at: new Date(Date.now() - 2 * 24 * 3600_000).toISOString(),
    });
    await mockRunsList(page, (url) => {
      const includeKilled = url.searchParams.get("include_killed") === "1";
      return includeKilled
        ? { runs: [killedRun], hiddenOlder: 0, hiddenKilled: 0 }
        : { runs: [], hiddenOlder: 0, hiddenKilled: 1 };
    });
    await gotoConsole(page);

    await expect(page.getByText("1 killed run is hidden", { exact: false })).toBeVisible();
    await expect(page.getByText("Killed 2 days ago")).toHaveCount(0);

    await page.getByRole("button", { name: "Include killed" }).click();
    await expect(page).toHaveURL(/include_killed=1/);
    // A 2-day-old row lands in "Earlier this week" (collapsed by default,
    // design.md §1) — its count going from absent to 1 is what "Include
    // killed" actually did; expand to see the row itself.
    const earlier = page.getByRole("button", { name: /Earlier this week/ });
    await expect(earlier).toBeVisible();
    await earlier.click();
    await expect(page.getByText("Killed 2 days ago")).toBeVisible();
  });

  // Review F1: real per-keystroke typing (`pressSequentially`, not `fill()`'s
  // one input event) — the exact shape that lost focus/characters after one
  // keystroke, since each keystroke's own filter-driven refetch used to
  // unmount the whole page including this input.
  test("search keeps focus and every character while typing one at a time", async ({ page }) => {
    await mockRunsList(page, (url) => ({
      runs: url.searchParams.get("q") === "abc" ? [baseRun("r1", "abc task")] : [baseRun("r0", "unrelated task")],
    }));
    await gotoConsole(page);
    await expect(page.getByText("unrelated task")).toBeVisible();

    const search = page.getByLabel("Search runs", { exact: true });
    await search.click();
    await search.pressSequentially("abc", { delay: 60 });
    await expect(search).toHaveValue("abc");
    await expect(search).toBeFocused();
    await expect(page.getByText("abc task")).toBeVisible();
  });

  test("no match, then Clear restores the default filters", async ({ page }) => {
    await mockRunsList(page, (url) => ({
      runs: url.searchParams.has("q") ? [] : [baseRun("r1", "The only run")],
    }));
    await gotoConsole(page);
    await expect(page.getByText("The only run")).toBeVisible();

    const search = page.getByLabel("Search runs", { exact: true });
    await search.fill("zzz-no-such-run");
    await expect(page.getByText("No runs match these filters.")).toBeVisible();
    await expect(page.getByText("Try a different search term or filter.")).toBeVisible();

    await page.getByRole("button", { name: "Clear filters" }).click();
    await expect(page.getByText("The only run")).toBeVisible();
    await expect(page).toHaveURL(/\/runs$/);
  });

  test("400px: no horizontal scroll", async ({ page }) => {
    await mockRunsList(page, () => ({
      runs: [
        // The review's own repro (F3) used an ado_consent row — its long
        // sign-in sentence is what actually pushes the title to 0px on a
        // fixed 3-column grid; a shorter word doesn't leave enough room to
        // reproduce it.
        baseRun("a1", "A long enough task title to stress the row at 400 pixels wide", {
          state: "RUNNING",
          attention: { kind: "ado_consent", by: "you", pending: 1 },
        }),
        baseRun("r1", "example-org/a-fairly-long-workspace-name", { state: "RUNNING", repo: "example-org/a-fairly-long-workspace-name" }),
      ],
    }));
    // gotoConsole's own sidebar-settle wait needs the full-width nav (the
    // shell collapses to a hamburger menu below its breakpoint) — resize
    // AFTER landing, same order every other narrow-viewport case in this
    // codebase uses (e.g. runs.spec.ts's failure-hint-chip narrow cases).
    await gotoConsole(page);
    await page.setViewportSize({ width: 400, height: 800 });
    await expect(page.getByRole("heading", { name: "Runs", level: 1 })).toBeVisible();
    await expect(page.getByText("A long enough task title", { exact: false })).toBeVisible();

    const overflow = await page.evaluate(() => ({
      scrollWidth: document.documentElement.scrollWidth,
      clientWidth: document.documentElement.clientWidth,
    }));
    expect(overflow.scrollWidth).toBeLessThanOrEqual(overflow.clientWidth);

    // Review F3: the title must not collapse to 0px — the side column moves
    // under it at this width (design.md §5, mock :176) instead of sharing
    // its row and shrinking it away.
    const titleLink = page.getByRole("link", { name: /A long enough task title/ });
    const box = await titleLink.boundingBox();
    expect(box).not.toBeNull();
    expect(box!.width).toBeGreaterThan(100);
  });

  // No pixel-diff gate in this repo (screenshots/docs.spec.ts's own header:
  // diffs flake, review is manual) — these render each theme and assert the
  // page came up correctly (the amber/red words theme-contrast.test.ts pins
  // for contrast are exactly what a reviewer needs visible in the capture),
  // then save a PNG for manual review rather than asserting pixels.
  for (const theme of ["light", "dark"] as const) {
    test(`renders correctly in the ${theme} theme (screenshot for manual review)`, async ({ page }) => {
      await page.addInitScript((t) => localStorage.setItem("wardyn-theme", t), theme);
      await mockRunsList(page, () => ({
        runs: [
          baseRun("a1", "Needs a decision", { state: "RUNNING", attention: { kind: "approval", by: "you", pending: 1 } }),
          baseRun("e1", "Failed today", { state: "FAILED", ended_at: new Date().toISOString() }),
        ],
      }));
      await gotoConsole(page);
      await expect(page.getByText("Needs a decision")).toBeVisible();
      await expect(page.getByText("Failed today")).toBeVisible();
      const isDark = await page.evaluate(() => document.documentElement.classList.contains("dark"));
      expect(isDark).toBe(theme === "dark");
      await page.screenshot({ path: `test-results/runs-landing-${theme}.png` });
    });
  }
});
