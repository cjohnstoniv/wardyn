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
// The board groups runs into "Needs your attention" (FAILED +
// WAITING_FOR_CONFIRMATION), "Active" (non-terminal, non-attention), and "Done"
// (terminal, grouped by outcome). State is asserted via the RunStatusBadge TEXT
// (run-status-badge.tsx / primitives.tsx), never CSS classes:
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
  // A table data row is a clickable <TableRow role="button"> (runs.tsx), not a
  // role="row" — only the header keeps the row role. Match the button whose
  // subtree carries the task text, scoped to the table so board-view cards
  // (also role="button") never match. The nested "Run actions" button doesn't
  // contain the task text, so the filter selects exactly the row.
  return page.getByRole("table").getByRole("button").filter({ hasText: task });
}

test.describe("Runs board (default view)", () => {
  test("boots into the board with attention / active / done sections", async ({ page }) => {
    await openRuns(page);

    // The three board sections render (SectionHeading <h2>s).
    await expect(page.getByRole("heading", { name: "Needs your attention" })).toBeVisible();
    await expect(page.getByRole("heading", { name: "Active" })).toBeVisible();
    await expect(page.getByRole("heading", { name: "Done" })).toBeVisible();

    // Every seeded run's task is on the board (all 9 render, grouped).
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

    // The RunDetail summary header carries the task (h1) + the RUNNING badge.
    await expect(page.getByRole("heading", { name: "e2e fixture 2", level: 1 })).toBeVisible();
    await expect(page.getByText("Running", { exact: true })).toBeVisible();

    // The Overview identity card renders the run's real fields.
    await expect(page.getByRole("heading", { name: "Identity" })).toBeVisible();
    await expect(page.getByText("Run ID")).toBeVisible();
    await expect(page.getByText("Repository")).toBeVisible();

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
    // are role="button" (see runRow), so count those, not role="row".
    await expect(
      page.getByRole("table").getByRole("button").filter({ hasText: /e2e fixture \d/ }),
    ).toHaveCount(9);
  });
});
