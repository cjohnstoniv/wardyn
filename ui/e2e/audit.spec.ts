/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, gotoConsole, navTo } from "./fixtures";
import type { Locator, Page } from "@playwright/test";

// Audit screen e2e (src/app/components/screens/audit.tsx).
//
// The redesign replaced the old columnar table with a DAY-GROUPED event stream:
// each row renders a plain-language verb (describeEvent) rather than the raw
// dotted action, the facets are Radix <Select>s (Event kind + Actor) instead of
// pill buttons, a count badge reports the loaded window, and clicking a run's id
// chip drills into that run's authoritative trail (a DrillBanner + server re-query).
//
// The seeded backend produces a small, deterministic trail purely from the seed:
// 9 `run.create` (rendered "Created the run"), 9 `identity.mint` ("Minted a
// workload identity"), and 1 `secret.write` ("Stored a secret"). Crucially, this
// admin-token backend is NOT in local mode, so EVERY seeded event is actor_type
// = system (the creator is attributed as the non-human admin-token actor, and
// identity mints + secret writes are inherently system) — so the Human and Agent
// actor facets are empty here. Exact event COUNTS depend on backend dispatch
// details, so these specs assert relative/structural behavior, never a fixed N.

// The count badge: "<n> event(s)", shown only when status === "ready".
function eventCount(page: Page): Locator {
  return page.getByText(/^\d+ events?$/);
}

// Open one of the two facet <Select>s (labelled "Event" / "Actor") and pick an
// option by its visible label.
async function selectFacet(page: Page, facet: "Event" | "Actor", option: string) {
  await page.getByRole("combobox", { name: facet }).click();
  await page.getByRole("option", { name: option, exact: true }).click();
}

// A row's plain-language verb (substring match; each verb is followed by its
// target). Scoped to <main> so the sidebar never interferes.
function verb(page: Page, text: string): Locator {
  return page.locator("main").getByText(text);
}

test.beforeEach(async ({ page }) => {
  await gotoConsole(page, "admin");
  await navTo(page, "Audit");
  await expect(page.getByRole("heading", { name: "Audit" })).toBeVisible();
});

test("renders the seeded, day-grouped audit trail with append-only framing", async ({ page }) => {
  // Wait for the loaded window: the count badge appears once status is ready.
  await expect(eventCount(page)).toBeVisible();

  // Append-only framing + the live-tail chip.
  await expect(page.getByText(/Append-only\./i)).toBeVisible();
  await expect(page.getByText("Live · appending")).toBeVisible();

  // The three seeded action kinds render as plain-language verbs (not raw
  // dotted actions).
  await expect(verb(page, "Stored a secret")).toBeVisible();
  await expect(verb(page, "Created the run").first()).toBeVisible();
  await expect(verb(page, "Minted a workload identity").first()).toBeVisible();

  // Events are grouped by day; all seeded events are recent, so the "Today"
  // group header is present.
  await expect(page.getByText("Today")).toBeVisible();

  // Append-order regression guard: the stream renders in the server's
  // authoritative order, never client re-sorted (audit.tsx's never-resort
  // invariant). The global window is newest-first, so the FIRST verb row is
  // the LAST-appended seed event — the single secret.write.
  await expect(
    page
      .locator("main")
      .getByText(/Stored a secret|Created the run|Minted a workload identity/)
      .first(),
  ).toContainText("Stored a secret");
});

// C-02 (#1062): scripts/e2e-backend.sh's "e2e fixture recording-history" run
// alone seeds 1000+ audit rows, so the deployment's total now legitimately
// exceeds LIST_LIMIT (1000, not the 500 this test used to name — that number
// was already stale) — the global (admin) window is newest-first and shares
// the SAME row cap a per-run trail does, so it now genuinely truncates. This
// used to assert the opposite (absent below the cap); the cap is real now.
test("the truncated indicator appears once the global window exceeds the 1000-row cap", async ({ page }) => {
  await expect(eventCount(page)).toBeVisible();
  await expect(page.getByText(/Showing the first 1000 events/i)).toBeVisible();
  await expect(page.getByText(/\(truncated\)/i)).toBeVisible();
});

test("the Event facet narrows by kind (Credentials vs Lifecycle)", async ({ page }) => {
  await expect(eventCount(page)).toBeVisible();

  // Credentials bucket = identity.mint + secret.write; run.create is lifecycle.
  await selectFacet(page, "Event", "Credentials");
  await expect(verb(page, "Minted a workload identity").first()).toBeVisible();
  await expect(verb(page, "Stored a secret")).toBeVisible();
  await expect(verb(page, "Created the run")).toHaveCount(0);

  // Lifecycle keeps run.create and drops the credential-family events.
  await selectFacet(page, "Event", "Lifecycle");
  await expect(verb(page, "Created the run").first()).toBeVisible();
  await expect(verb(page, "Minted a workload identity")).toHaveCount(0);

  // Restore the full window.
  await selectFacet(page, "Event", "All events");
  await expect(verb(page, "Created the run").first()).toBeVisible();
  await expect(verb(page, "Minted a workload identity").first()).toBeVisible();
});

// C-02 (#1062): before this fixture, every seeded event was actor_type=system
// (this test used to assert Human/Agent were both empty). The
// "recording-history" run is now the ONLY source of human- and
// agent-attributed rows anywhere in this seed — its 2 recording events
// (actor_type=human) and its 1001 filler egress.allow rows (actor_type=agent)
// — so System no longer accounts for the WHOLE window, and Human/Agent are no
// longer empty. The facet itself filters client-side over the same capped
// global fetch every other assertion here already shares (audit.tsx's
// actorFilter), so this only changes what that fixed window now contains.
test("the Actor facet narrows: System no longer keeps everything, and C-02's fixture is the sole human/agent source", async ({
  page,
}) => {
  await expect(eventCount(page)).toBeVisible();
  const fullCount = await eventCount(page).innerText();

  await selectFacet(page, "Actor", "System");
  await expect(eventCount(page)).not.toHaveText(fullCount);
  await expect(verb(page, "Minted a workload identity").first()).toBeVisible();
  await expect(verb(page, "Created the run").first()).toBeVisible();

  // Exactly the recording-history fixture's two recording rows (alice + bob).
  await selectFacet(page, "Actor", "Human");
  await expect(eventCount(page)).toHaveText("2 events");
  await expect(page.getByText("No events match these filters.")).toHaveCount(0);

  // Whichever of the fixture's 1001 filler rows survived the global window's cap.
  await selectFacet(page, "Actor", "Agent");
  await expect(page.getByText("No events match these filters.")).toHaveCount(0);

  // 'All actors' restores the full window.
  await selectFacet(page, "Actor", "All actors");
  await expect(eventCount(page)).toHaveText(fullCount);
});

test("search filters by action / actor / target text", async ({ page }) => {
  await expect(eventCount(page)).toBeVisible();
  const search = page.getByPlaceholder("Search events, domains, run IDs…");

  // "identity.mint" matches the raw action of the mint events only (the rows
  // render the verb, but search reads the underlying action field).
  await search.fill("identity.mint");
  await expect(verb(page, "Minted a workload identity").first()).toBeVisible();
  await expect(verb(page, "Created the run")).toHaveCount(0);

  // "secret" matches the single secret.write event.
  await search.fill("secret");
  await expect(verb(page, "Stored a secret")).toBeVisible();

  // A term that matches nothing → empty state.
  await search.fill("zzz-no-such-event");
  await expect(page.getByText("No events match these filters.")).toBeVisible();
  await expect(eventCount(page)).toHaveText("0 events");

  await search.fill("");
  await expect(verb(page, "Stored a secret")).toBeVisible();
});

test("clicking a run drills into that run's trail, and 'Show all events' restores the window", async ({ page }) => {
  await expect(eventCount(page)).toBeVisible();
  const fullCount = await eventCount(page).innerText();

  // Each event that belongs to a run renders a small monospace run-id chip
  // (the ONLY font-mono <button> on the screen — no distinct accessible name to
  // target, so key on that stable class). Clicking it re-queries the server for
  // that run's authoritative trail.
  await page.locator("main button.font-mono").first().click();

  // The DrillBanner appears (its "Show all events" clear action + "Open run"
  // link are unique to it) and the window narrows below the full trail.
  await expect(page.getByRole("button", { name: "Show all events" })).toBeVisible();
  await expect(page.getByRole("link", { name: /Open run/ })).toBeVisible();
  await expect(eventCount(page)).not.toHaveText(fullCount);

  // The run's own trail carries its create + identity mint; the run-less
  // secret.write is not part of any single run's trail — its absence is the
  // cross-run no-leak guarantee.
  await expect(verb(page, "Created the run").first()).toBeVisible();
  await expect(verb(page, "Minted a workload identity").first()).toBeVisible();
  await expect(verb(page, "Stored a secret")).toHaveCount(0);

  // Clearing the drill re-queries the global window and the full trail returns.
  await page.getByRole("button", { name: "Show all events" }).click();
  await expect(page.getByRole("button", { name: "Show all events" })).toHaveCount(0);
  await expect(eventCount(page)).toHaveText(fullCount);
});
