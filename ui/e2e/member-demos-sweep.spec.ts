/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, asRealMember, asRealSecurityAdmin } from "./fixtures";
import type { Page } from "@playwright/test";
import { DEMOS, DEMO_IDS, type DemoId } from "../src/app/components/screens/demos/demo-catalog";

// M-6 sweep, real member token (asRealMember — a genuine seeded wdn_ token,
// never a spliced /me): the whole demo catalog, partitioned into the three
// buckets a member's Getting Started (member-getting-started.tsx) can put a
// demo in on THIS backend (real wardynd + Postgres + the `none` runner, no
// demo secret stored, and — forced below, review F2 — no connected model):
//
//  - VISIBLE: walkable here (no needsModel/needsSecret precondition unmet)
//    AND not narrowed by the member's default governance ceiling
//    (setup/steps.ts's ceilingNarrows — owner ruling 2026-09-25: a demo the
//    ceiling would rewrite is HIDDEN from a member entirely, never offered
//    watch-only).
//  - NARROWED: walkable, but the ceiling hides it (allow_all_egress,
//    wait_for_review, or an api_key/git_pat/ssh_key/cloud_sts grant).
//  - UNWALKABLE: dropped for EVERY role (needsModel with no connected model,
//    or needsSecret with none of the five demo secrets stored) — already the
//    operator-side gap demos.spec.ts proves; listed here only so the
//    partition below accounts for the full catalog.
//
// member-getting-started-demos.test.tsx already pins this selection at the
// component level, against a FABRICATED status. This sweep is the same
// selection proven end to end: a real /api/v1/setup/status redaction
// (internal/api/setup.go's redactSetupStatusForUser) combined with the
// client's own walkableDemos/ceilingNarrows produces exactly this set.
const VISIBLE = [
  "sealed-box",
  "fail-then-approve",
  "once-or-for-good",
  "write-only-by-design",
  "github-app-broker",
] as const;

const NARROWED = [
  "held-at-the-door",
  "lines-that-cant-be-crossed",
  "denied-however-spelled",
  "record-a-policy",
  "sts-fail-closed",
] as const;

const UNWALKABLE = [
  "agent-in-the-box",
  "key-never-in-the-box",
  "authorized-not-issued",
  "rest-api-token",
  "pat-stdout-only",
  "ssh-briefly-resident",
] as const;

// The catalog's own titles, not re-typed here — a row renders `demo.title`
// verbatim (member-getting-started.tsx's DemoRow), so this is what a NARROWED
// or UNWALKABLE id's absence actually has to prove is missing.
const TITLE = Object.fromEntries(DEMOS.map((d) => [d.id, d.title])) as Record<DemoId, string>;

// Review F2: the harness demo reads the server's own llm_ready
// (readiness.ts's hasLlmPath), a fact about the HOST, not about the member
// ceiling this spec exists to sweep. Forcing it false here (on top of the
// real, already-redacted response — nothing else about the member's real
// session or authorization changes) is what makes the UNWALKABLE bucket a
// fact about the catalog.
async function forceNoModelAccess(page: Page): Promise<void> {
  let cached: Record<string, unknown> | null = null;
  await page.route("**/api/v1/setup/status*", async (route) => {
    if (!cached) {
      const body = (await (await route.fetch()).json()) as Record<string, unknown>;
      body.llm_ready = false;
      cached = body;
    }
    await route.fulfill({ json: cached });
  });
}

// Review F1: the demo list is empty until /api/v1/setup/status resolves
// (walkableDemos(null) is [], setup/steps.ts), and DemoDetail is
// React.lazy — so a `toHaveCount(0)` negative check run right after the
// page's OWN heading (which paints on first render, before that fetch lands)
// can pass for a reason that has nothing to do with the ceiling: the list
// simply hasn't rendered yet. Waiting for a title that IS expected to show —
// one per section, so both egressDemos and secretsDemos are proven computed —
// is a positive signal that the whole selection has settled, member ceiling
// included, before any negative assertion runs.
async function waitForMemberDemosLoaded(page: Page): Promise<void> {
  await expect(page.getByText(TITLE["sealed-box"], { exact: true })).toBeVisible();
  await expect(page.getByText(TITLE["write-only-by-design"], { exact: true })).toBeVisible();
}

test.describe("member demos sweep (#685 M-6, real member token)", () => {
  test.beforeEach(async ({ page }) => {
    await asRealMember(page);
    await forceNoModelAccess(page);
  });

  for (const id of VISIBLE) {
    test(`${id} reaches the member, Start closed only for -runner-none`, async ({ page }) => {
      await page.goto(`/setup?step=${id}`);
      const card = page.getByTestId(`demo-card-${id}`);
      await expect(card).toBeVisible();
      await expect(page.getByTestId(`demo-start-${id}`)).toBeDisabled();
      // Review F3: "disabled" alone doesn't say WHY — a hidden render error
      // or member-inappropriate (admin-only) content could disable it too,
      // and still leave the card looking fine at a glance. Pin the actual
      // reason (same hint demos.spec.ts pins for an operator: no runner) and
      // that nothing an operator-only branch would render leaked to a member.
      await expect(card.getByTestId("demos-step-not-ready")).toBeVisible();
      // Page-scoped, not card-scoped: the row's own chrome (title, Open/Close)
      // sits OUTSIDE demo-card-<id>'s own wrapper, and only this one demo is
      // ever open on the page (the ?step= deep link), so anything either
      // selector matches anywhere on the page belongs to this row.
      await expect(page.locator('[role="alert"]')).toHaveCount(0);
      await expect(page.locator('a[href^="/admin"]')).toHaveCount(0);
    });
  }

  for (const id of [...NARROWED, ...UNWALKABLE]) {
    test(`${id} never reaches a member's Getting Started`, async ({ page }) => {
      await page.goto(`/setup?step=${id}`);
      await waitForMemberDemosLoaded(page);
      // A deep link this page doesn't offer opens nothing and errors
      // nothing (the same contract member-getting-started-demos.test.tsx
      // pins) — the row's own title, not only the lazy card testid, since
      // that title is what actually renders first if the ceiling ever
      // stopped hiding the row.
      await expect(page.getByText(TITLE[id], { exact: true })).toHaveCount(0);
      await expect(page.getByTestId(`demo-card-${id}`)).toHaveCount(0);
    });
  }

  // Guards the partition itself: every id DEMO_IDS carries today is assigned
  // to exactly one of the three buckets above, so a new demo added to the
  // catalog with no decision made here fails loudly instead of silently
  // passing (it would appear in DEMO_IDS but neither list).
  test("the three buckets above account for the whole catalog, once each", () => {
    const swept = [...VISIBLE, ...NARROWED, ...UNWALKABLE].sort();
    expect(swept).toEqual([...DEMO_IDS].sort());
  });

  // Review F5: /demos is a redirect, role-agnostic (App.tsx) — it has to
  // land a member on their OWN Getting Started, not the admin funnel it used
  // to precede.
  test("the /demos redirect lands a member on their own Getting Started", async ({ page }) => {
    await page.goto("/demos");
    await expect(page).toHaveURL(/\/setup\?step=sealed-box/);
    await waitForMemberDemosLoaded(page);
    await expect(page.getByTestId("demo-card-sealed-box")).toBeVisible();
  });
});

// Review F5: onboarding-screen.tsx routes /setup to MemberGettingStarted for
// every role but admin — a security admin gets the identical member page a
// user does, never the operator funnel it cannot act on.
test.describe("a security admin reaches the same member Getting Started (#685 M-6)", () => {
  test.beforeEach(async ({ page }) => {
    await asRealSecurityAdmin(page);
    await forceNoModelAccess(page);
  });

  test("sees the member page and a keyless demo, not the admin funnel", async ({ page }) => {
    await page.goto("/setup?step=sealed-box");
    await expect(page.getByRole("heading", { name: "What's set up for you" })).toBeVisible();
    await expect(page.getByText("Pick your barrier")).toHaveCount(0);
    await expect(page.getByTestId("demo-card-sealed-box")).toBeVisible();
  });
});
