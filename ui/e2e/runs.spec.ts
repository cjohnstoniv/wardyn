/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, gotoConsole, navTo, sidebarLink } from "./fixtures";
import type { Page, Locator } from "@playwright/test";

// ============================================================================
// Runs lane: the UNIFIED Runs screen (board + table views, merged from the old
// Runs table and the retired Fleet board) + the addressable /runs/:id RunDetail
// hub + kill.
//
// The seeded backend creates 9 runs, one per RunState, with deterministic task
// text "e2e fixture 0".."e2e fixture 8" mapped to states by creation order:
//   fixture 0 -> PENDING                  (active)
//   fixture 1 -> STARTING                 (active)
//   fixture 2 -> RUNNING                  (active)
//   fixture 3 -> WAITING_FOR_CONFIRMATION (needs attention)
//   fixture 4 -> COMPLETED                (done)      <-- the critical regression
//   fixture 5 -> STOPPED                  (done)
//   fixture 6 -> FAILED                   (needs attention)
//   fixture 7 -> KILLED                   (done)
//   fixture 8 -> ARCHIVED                 (done)
//
// The board groups runs by TITLE. The seeder gives fixtures 0 and 1 the SAME
// title ("e2e group") so there is a real group to render — a title held by one
// run is not a group — and leaves the rest untitled, which is the legacy / CLI /
// system-run shape that must keep rendering by task. Triage survives the change
// as the state facet, attention-first ordering, and per-state counts in each
// group header. State is asserted via the RunStateBadge TEXT
// (primitives.tsx), never CSS classes:
//   PENDING "Pending", STARTING "Starting", RUNNING "Running",
//   WAITING_FOR_CONFIRMATION "Awaiting confirmation", COMPLETED "Completed",
//   STOPPED "Stopped", FAILED "Failed", KILLED "Killed", ARCHIVED "Archived".
// Barriers render as the user labels Fence/Wall/Vault (ConfinementChip) — the
// wire codes CC1/CC2/CC3 never leak as visible text.
// ============================================================================

// Run this file's tests serially in a single worker. The suite shares one
// seeded backend and the final tests MUTATE run state (kill). Serial mode keeps
// the read-only assertions from racing the mutation regardless of the global
// fullyParallel setting, and runs the mutating tests last (declaration order).
test.describe.configure({ mode: "serial" });

// fixture index -> { state badge label, terminal? }
const FIXTURES = [
  { task: "e2e fixture 0", badge: "Pending", terminal: false },
  { task: "e2e fixture 1", badge: "Starting", terminal: false },
  { task: "e2e fixture 2", badge: "Running", terminal: false },
  { task: "e2e fixture 3", badge: "Awaiting confirmation", terminal: false },
  { task: "e2e fixture 4", badge: "Completed", terminal: true },
  { task: "e2e fixture 5", badge: "Stopped", terminal: true },
  { task: "e2e fixture 6", badge: "Failed", terminal: true },
  { task: "e2e fixture 7", badge: "Killed", terminal: true },
  { task: "e2e fixture 8", badge: "Archived", terminal: true },
] as const;

// Open the Runs screen and wait for the seeded board to render (board is the
// default density; PENDING fixture 0 is always present).
async function openRuns(page: Page): Promise<void> {
  await gotoConsole(page);
  await navTo(page, "Runs");
  await expect(page.getByRole("heading", { name: "Runs", level: 1 })).toBeVisible();
  await expect(page.getByText("e2e fixture 0")).toBeVisible();
}

// Switch to the dense Table view (a real <table>, so rows are role-addressable).
async function switchToTable(page: Page): Promise<void> {
  await page.getByRole("button", { name: "Table" }).click();
  await expect(page.getByRole("table")).toBeVisible();
}

// A table body row scoped by its task text (the header row has no fixture text).
function runRow(page: Page, task: string): Locator {
  // fix: TableRow dropped its role="button" override — a role="button" row
  // wrapping the real per-row action buttons (RunActions) was an invalid
  // nested-interactive-widget ARIA structure (same fix as the board's
  // RunCard container; see runs.tsx's RunsTable). A <tr> inside a real
  // <table> keeps its native "row" role instead. Match the row whose subtree
  // carries the task text, scoped to the table so board-view cards never
  // match (they're plain <div>s, not table rows, so getByRole("table") alone
  // already excludes them).
  return page.getByRole("table").getByRole("row").filter({ hasText: task });
}

test.describe("Runs board (default view)", () => {
  test("boots into the board with runs grouped by title", async ({ page }) => {
    await openRuns(page);

    // Fixtures 0 and 1 share a title, so they render under one group header.
    // The rest are untitled and fall into the trailing ungrouped grid.
    const group = page.getByRole("region", { name: "e2e group" });
    await expect(group).toBeVisible();
    await expect(group.getByText("e2e fixture 0")).toBeVisible();
    await expect(group.getByText("e2e fixture 1")).toBeVisible();
    await expect(page.getByRole("heading", { name: "Ungrouped" })).toBeVisible();

    // Every seeded run is still on the board — an untitled run names itself by
    // its task exactly as it did before titles existed.
    for (const f of FIXTURES) {
      await expect(page.getByText(f.task)).toBeVisible();
    }

    // The attention section surfaces the WAITING_FOR_CONFIRMATION + FAILED runs.
    await expect(page.getByText("Awaiting confirmation")).toBeVisible();
    await expect(page.getByText("Failed", { exact: true })).toBeVisible();
  });

  test("the table view lists all nine runs with their state badges", async ({ page }) => {
    await openRuns(page);
    await switchToTable(page);

    // Column headers prove the table chrome rendered (task lives under "Run",
    // state under "State", the barrier under "Barrier").
    const table = page.getByRole("table");
    await expect(table.getByRole("columnheader", { name: "Run", exact: true })).toBeVisible();
    await expect(table.getByRole("columnheader", { name: "State" })).toBeVisible();
    await expect(table.getByRole("columnheader", { name: "Barrier" })).toBeVisible();

    // Every seeded run is present with a state badge. Terminal states are
    // stable, so those assert the exact badge; non-terminal seeded states
    // (Pending/Starting/Running/…) may legitimately be advanced by the
    // backend's reconciler between seed and render (e.g. PENDING → FAILED on
    // the none-runner), so those assert the row carries SOME known state
    // badge rather than pinning a racy one.
    const anyBadge = new RegExp(`^(${FIXTURES.map((f) => f.badge).join("|")})$`);
    for (const f of FIXTURES) {
      const row = runRow(page, f.task);
      await expect(row).toBeVisible();
      if (f.terminal) {
        await expect(row.getByText(f.badge, { exact: true })).toBeVisible();
      } else {
        await expect(row.getByText(anyBadge).first()).toBeVisible();
      }
    }
  });

  test("search filters the board down to a single matching run", async ({ page }) => {
    await openRuns(page);

    const search = page.getByPlaceholder("Search runs, repos, IDs…");
    await search.fill("e2e fixture 4");

    // Only the COMPLETED fixture-4 run should remain.
    await expect(page.getByText("e2e fixture 4")).toBeVisible();
    await expect(page.getByText("e2e fixture 0")).toHaveCount(0);
  });

  test("a non-matching search shows the empty state, then recovers when cleared", async ({ page }) => {
    await openRuns(page);

    const search = page.getByPlaceholder("Search runs, repos, IDs…");
    await search.fill("zzz-no-such-run-zzz");

    // EmptyState for a query renders this copy (runs.tsx).
    await expect(page.getByText("No runs match these filters.")).toBeVisible();
    await expect(page.getByText("Try a different search term or facet.")).toBeVisible();

    // Clearing the filters restores the full board.
    await page.getByRole("button", { name: "Clear filters" }).click();
    await expect(page.getByText("e2e fixture 0")).toBeVisible();
  });

  // The critical regression: a COMPLETED run must NOT crash the console.
  test("a COMPLETED run renders without crashing the console", async ({ page }) => {
    await openRuns(page);

    // The COMPLETED run + its "Completed" badge render, and the console chrome
    // (sidebar) stays intact — i.e. no blank crash screen.
    await expect(page.getByText("e2e fixture 4")).toBeVisible();
    // "Completed" appears as both the board's section header and the run's
    // status badge — both expected, so first() avoids strict mode.
    await expect(page.getByText("Completed", { exact: true }).first()).toBeVisible();
    await expect(page.getByText("e2e fixture 8")).toBeVisible();
    await expect(sidebarLink(page, "Runs")).toBeVisible();
  });
});

test.describe("Run detail (/runs/:id)", () => {
  test("clicking a run opens its addressable detail hub with identity + kill", async ({ page }) => {
    await openRuns(page);

    // Clicking the run card navigates to the addressable /runs/:id page (the old
    // slide-over Sheet is gone).
    await page.getByText("e2e fixture 2").click();
    await expect(page).toHaveURL(/\/runs\/.+/);

    // The command bar carries the task (h1) + the RUNNING badge.
    await expect(page.getByRole("heading", { name: "e2e fixture 2", level: 1 })).toBeVisible();
    await expect(page.getByText("Running", { exact: true })).toBeVisible();

    // The run's real identity fields still render — but the cockpit split them:
    // the repo moved UP into the 52px command bar (it is one of the facts that
    // must be visible without scrolling), while the run id / SPIFFE id stayed in
    // the Identity widget on the evidence rail. Same facts, two homes.
    await expect(page.getByRole("heading", { name: "Identity" })).toBeVisible();
    await expect(page.getByText("acme/widgets")).toBeVisible();
    await expect(page.getByText("Run", { exact: true })).toBeVisible();

    // The terminal is the hero and it is ABOVE THE FOLD — the whole point of the
    // redesign. Assert it geometrically, not just that it rendered: the old
    // screen also "rendered" a terminal, ~1,120px down the page.
    const pane = page.getByTestId("run-terminal-pane");
    await expect(pane).toBeVisible();
    const box = await pane.boundingBox();
    expect(box).not.toBeNull();
    expect(box!.y).toBeLessThan(300);

    // And the page itself does not scroll: the cockpit fills the viewport.
    const scrolls = await page.evaluate(() => {
      const m = document.querySelector("main");
      return !!m && m.scrollHeight > m.clientHeight;
    });
    expect(scrolls).toBe(false);

    // A non-terminal run has an enabled danger-zone Kill button.
    const killBtn = page.getByRole("button", { name: "Kill", exact: true });
    await expect(killBtn).toBeVisible();
    await expect(killBtn).toBeEnabled();
  });

  test("detail of a COMPLETED run renders and has a disabled Kill button", async ({ page }) => {
    await openRuns(page);
    await page.getByText("e2e fixture 4").click();
    await expect(page).toHaveURL(/\/runs\/.+/);

    await expect(page.getByRole("heading", { name: "e2e fixture 4", level: 1 })).toBeVisible();
    await expect(page.getByText("Completed", { exact: true })).toBeVisible();

    // Terminal run => the Kill trigger is disabled.
    const killBtn = page.getByRole("button", { name: "Kill", exact: true });
    await expect(killBtn).toBeVisible();
    await expect(killBtn).toBeDisabled();
  });
});

test.describe("Kill availability via the row dropdown (table)", () => {
  // Open a table row's "..." action menu and return the "Kill run" menuitem.
  async function killMenuItem(page: Page, task: string): Promise<Locator> {
    await runRow(page, task).getByRole("button", { name: "Run actions" }).click();
    const menu = page.getByRole("menu");
    await expect(menu).toBeVisible();
    return menu.getByRole("menuitem", { name: "Kill run" });
  }

  test("Kill run is enabled in the menu for active runs", async ({ page }) => {
    await openRuns(page);
    await switchToTable(page);
    const killItem = await killMenuItem(page, "e2e fixture 2"); // RUNNING
    await expect(killItem).toBeVisible();
    // Radix marks a disabled DropdownMenuItem with aria-disabled; active => not.
    await expect(killItem).not.toHaveAttribute("aria-disabled", "true");
  });

  test("Kill run is disabled in the menu for every terminal run", async ({ page }) => {
    await openRuns(page);
    await switchToTable(page);

    for (const f of FIXTURES.filter((x) => x.terminal)) {
      const killItem = await killMenuItem(page, f.task);
      await expect(killItem).toBeVisible();
      await expect(killItem).toHaveAttribute("aria-disabled", "true");
      // Close the menu before the next iteration.
      await page.keyboard.press("Escape");
      await expect(page.getByRole("menu")).toHaveCount(0);
    }
  });
});

test.describe("Killing an active run", () => {
  // Mutating: kills a currently-ACTIVE run from its detail page. The victim is
  // picked at test time via the API — seeded non-terminal states can be
  // advanced by the reconciler before this (serial-last) test runs, PENDING
  // especially, so pinning one fixture is racy. On the `none` runner the run
  // has no live process, so the backend 409s the kill yet reconciles the row
  // to a terminal state. The UI's error-handling fix turns that rejection into
  // a "Failed to kill" toast (instead of an unhandled rejection that blanks
  // the console) and always re-syncs the run — we assert EITHER toast variant:
  // the point is the failure is surfaced, not swallowed.
  const ACTIVE_BADGE: Record<string, string> = {
    PENDING: "Pending",
    STARTING: "Starting",
    RUNNING: "Running",
    WAITING_FOR_CONFIRMATION: "Awaiting confirmation",
  };

  test("killing an active run from its detail page surfaces a toast and reconciles", async ({ page }) => {
    const resp = await page.request.get("/api/v1/runs", {
      headers: { Authorization: "Bearer wardyn-e2e-token" },
    });
    expect(resp.ok()).toBeTruthy();
    const payload = (await resp.json()) as unknown;
    const list = (
      Array.isArray(payload) ? payload : ((payload as { runs?: unknown[] }).runs ?? [])
    ) as Array<{ state: string; task: string }>;
    // Prefer the states observed stable under the reconciler.
    const victim = ["WAITING_FOR_CONFIRMATION", "RUNNING", "STARTING", "PENDING"]
      .map((s) => list.find((r) => r.state === s && /e2e fixture/.test(r.task)))
      .find(Boolean);
    expect(victim, "no active seeded run left to kill").toBeTruthy();
    const badge = ACTIVE_BADGE[victim!.state];

    await openRuns(page);
    await page.getByText(victim!.task).click();
    await expect(page).toHaveURL(/\/runs\/.+/);
    await expect(page.getByText(badge, { exact: true })).toBeVisible();

    const killBtn = page.getByRole("button", { name: "Kill", exact: true });
    await expect(killBtn).toBeEnabled();
    await killBtn.click();

    const confirm = page.getByRole("alertdialog");
    await expect(confirm).toBeVisible();
    await expect(confirm.getByText(/Kill .+\?/)).toBeVisible();
    await confirm.getByRole("button", { name: "Kill run" }).click();

    // Success on a graceful kill, or "Failed to kill" on a 409 from the `none`
    // runner. Either proves the rejection was handled, not swallowed.
    await expect(page.getByText(/Kill requested for|Failed to kill/).first()).toBeVisible({
      timeout: 10000,
    });

    // The detail page polls + re-syncs the run after the kill; the badge no
    // longer reads the old active label (the run reconciled to a terminal state),
    // and the page is still rendered (not blanked by a crash).
    await expect(page.getByText(badge, { exact: true })).toHaveCount(0, { timeout: 10000 });
    await expect(page.getByRole("heading", { name: victim!.task, level: 1 })).toBeVisible();
  });

  test("the runs list still shows all nine runs after a kill + manual refresh", async ({ page }) => {
    await openRuns(page);
    // The Refresh button re-fetches the list (aria-label "Refresh now").
    await page.getByRole("button", { name: "Refresh now" }).click();
    await switchToTable(page);
    // Still nine runs total (a kill changes a state, not the count). Data rows
    // are role="row" (see runRow) — the header row has no fixture text, so
    // the filter excludes it without scoping to TableBody.
    await expect(
      page.getByRole("table").getByRole("row").filter({ hasText: /e2e fixture \d/ }),
    ).toHaveCount(9);
  });
});

// R4-F002: the console used to pull the WHOLE fleet's approvals and filter in
// the browser — i.e. AFTER the server's requested_at DESC window — so past
// LIST_LIMIT lifetime approvals a run's own holds vanished from its own detail
// page (internal/api/approvals.go:56-61 spells out why the predicate has to run
// server-side). jsdom pins the call args; only a real browser proves the wire.
test.describe("Run detail — approvals are scoped on the wire", () => {
  test("GET /approvals carries ?run_id= for the run being viewed", async ({ page }) => {
    const approvalRequests: string[] = [];
    page.on("request", (req) => {
      const url = req.url();
      if (req.method() === "GET" && /\/api\/v1\/approvals(\?|$)/.test(url)) {
        approvalRequests.push(url);
      }
    });

    await openRuns(page);
    await page.getByText("e2e fixture 2").click();
    await expect(page).toHaveURL(/\/runs\/.+/);
    await expect(page.getByRole("heading", { name: "e2e fixture 2", level: 1 })).toBeVisible();

    const runId = new URL(page.url()).pathname.split("/").pop() ?? "";
    expect(runId).not.toEqual("");

    await expect
      .poll(() => approvalRequests.some((u) => u.includes(`run_id=${runId}`)))
      .toBe(true);
    // (The shell's own attention poll — App.tsx's fleet-wide PENDING count — is
    // legitimately un-scoped and keeps ticking here, so "no un-scoped read at
    // all" is not the assertion; "this page asks for THIS run" is.)
  });
});


// R4-F020: the Add-widget catalog enumerated the whole registry while the grid
// drew only what `available` admitted. On a FINISHED run the ssh widget can
// never render, yet the catalog still offered "Attach from your terminal" —
// one click ticked the row, ran addWidget and PUT the phantom placement to
// /api/v1/me/run-layout, and no tile came back. canvas.test.tsx pins the
// absence in jsdom; only a real browser proves nothing is PERSISTED.
test.describe("Run cockpit — the layout catalog offers no dead controls", () => {
  test("a finished run's catalog omits the ssh widget, and persists nothing", async ({ page }) => {
    const layoutWrites: string[] = [];
    page.on("request", (req) => {
      if (req.method() === "PUT" && new URL(req.url()).pathname === "/api/v1/me/run-layout") {
        layoutWrites.push(req.postData() ?? "");
      }
    });

    await openRuns(page);
    await page.getByText("e2e fixture 4").click(); // Completed
    await expect(page).toHaveURL(/\/runs\/.+/);
    await expect(page.getByRole("heading", { name: "e2e fixture 4", level: 1 })).toBeVisible();

    await page.getByRole("button", { name: "Edit layout" }).click();
    await page.getByRole("button", { name: "Add widget" }).click();
    const catalog = page.getByRole("dialog");
    await expect(catalog).toBeVisible();

    await expect(catalog.getByRole("button", { name: "Attach from your terminal" })).toHaveCount(0);
    // The entries that CAN render are still offered.
    await expect(catalog.getByRole("button", { name: "Sandbox" })).toBeVisible();
    // Opening the catalog wrote nothing, and no phantom widget can have been
    // stored because none was offered.
    expect(layoutWrites.every((b) => !b.includes('"ssh"'))).toBe(true);
  });
});

// R4-F141: RunDetailScreen.load() awaited FIVE fetches with Promise.all, so any
// ONE of them rejecting replaced the whole cockpit with ErrorState's "We
// couldn't reach the Wardyn control plane" — an outage claim that is false when
// GET /runs/{id} answered 200, and one that takes the run's state, its terminal,
// its approvals strip and its KILL button with it. The rejection is routine, not
// hypothetical: handleListApprovals answers 500 "approval listing is not scoped
// for members on this backend" without ApprovalsByRunCreatorPager
// (internal/api/approvals.go), and a degraded audit store fails listAudit.
// run-detail.test.tsx pins the state machine; only a browser proves the page a
// human is left holding.
test.describe("Run detail — a failing side fetch is not an outage", () => {
  test("keeps the cockpit when /audit and /approvals both 500", async ({ page }) => {
    await openRuns(page);

    // Fail the two subsidiary reads for the detail page only; /runs/{id} is
    // left alone, which is the whole point — the run loaded.
    await page.route("**/api/v1/audit*", (route) =>
      route.fulfill({ status: 500, contentType: "application/json", body: '{"error":"audit store degraded"}' }),
    );
    await page.route("**/api/v1/approvals*", (route) =>
      route.fulfill({
        status: 500,
        contentType: "application/json",
        body: '{"error":"approval listing is not scoped for members on this backend"}',
      }),
    );

    await page.getByText("e2e fixture 2").click(); // RUNNING
    await expect(page).toHaveURL(/\/runs\/.+/);

    await expect(page.getByRole("heading", { name: "e2e fixture 2", level: 1 })).toBeVisible();
    await expect(page.getByText("We couldn't reach the Wardyn control plane. Please try again.")).toHaveCount(0);
    // The one control that ends a runaway run is still reachable.
    await expect(page.getByRole("button", { name: "Kill" })).toBeVisible();

    await page.unroute("**/api/v1/audit*");
    await page.unroute("**/api/v1/approvals*");
  });
});

// R4-F142: the canvas tile's FILL rule was written for a widget that renders ONE
// root card (`[&>section]:flex-1`), and the terminal widget renders a FRAGMENT —
// the M7(b) failure block, then the pane. The rule therefore caught the failure
// block, a `shrink-0` <section> built to size to its content, and gave it
// `flex: 1 1 0%`: measured in Chromium at 271px of a 518px tile, half the replay
// pane gone on every KILLED/FAILED run. jsdom computes no layout, so the vitest
// pin can only assert the selector — this is the one that reads real pixels.
test.describe("Run cockpit — the failure block sizes to its content, not to half the tile", () => {
  test("a killed run keeps its replay pane", async ({ page }) => {
    await openRuns(page);
    await page.getByText("e2e fixture 7").click(); // KILLED — always gets the block
    await expect(page).toHaveURL(/\/runs\/.+/);
    await expect(page.getByRole("heading", { name: "e2e fixture 7", level: 1 })).toBeVisible();

    const block = page.getByTestId("run-failure-block");
    await expect(block).toBeVisible();

    const measured = await block.evaluate((el) => {
      const cs = getComputedStyle(el);
      const parent = el.parentElement as HTMLElement;
      return {
        flexGrow: cs.flexGrow,
        blockH: el.getBoundingClientRect().height,
        scrollH: el.scrollHeight,
        parentH: parent.getBoundingClientRect().height,
      };
    });

    // Not stretched...
    expect(measured.flexGrow).toBe("0");
    // ...and not stretched in effect either: it is as tall as its own content
    // (within a rounding pixel), never a fixed share of the tile.
    expect(measured.blockH).toBeLessThanOrEqual(measured.scrollH + 2);
    // The replay pane below it is still rendered. (Not a share of the hero:
    // the widget body is the viewport's height, and a long failure detail can
    // legitimately be most of a 720px tile — the F142 property is the two
    // checks above, content-sized and never flex-stretched, measured in real
    // pixels; the ratio this asserted before was the author's viewport, not
    // the fix.)
    const replay = block.locator("xpath=following-sibling::*[1]");
    await expect(replay).toBeVisible();
    expect(measured.parentH).toBeGreaterThan(measured.blockH);
  });
});

// R4-F037: the attach card's OFF paragraphs are claims about the DEPLOYMENT
// ("Off on this deployment... an operator turns it on by setting
// WARDYN_SSH_LISTEN"). lib/api/health.ts swallows a non-ok /healthz into a
// resolved `{}`, so the card used to print both claims after a 503 and never
// re-check (the effect's deps never change on this page). The vitest pin drives
// the same code path with a stubbed fetch; this one drives a real browser
// against a real daemon whose /healthz is failing.
test.describe("Attach card — a failing /healthz claims nothing about the deployment", () => {
  test("prints neither lane's OFF copy while /healthz 503s", async ({ page }) => {
    await openRuns(page);
    // Installed BEFORE the run opens: the card asks once, on mount.
    await page.route("**/healthz", (route) =>
      route.fulfill({ status: 503, contentType: "application/json", body: '{"error":"unavailable"}' }),
    );

    await page.getByText("e2e fixture 2").click(); // RUNNING — the card's gate
    await expect(page).toHaveURL(/\/runs\/.+/);
    // The card still renders: the CLI lane needs no gateway at all.
    await expect(page.getByText("Attach from your terminal")).toBeVisible();
    await expect(page.getByText("Wardyn CLI")).toBeVisible();
    // ...and asserts nothing about the two lanes it got no answer for.
    await expect(page.getByText(/Off on this deployment/)).toHaveCount(0);
    // Both headings stay — the lanes exist, their state is simply unknown.
    await expect(page.getByText("UI apps")).toBeVisible();

    await page.unroute("**/healthz");
  });
});
