/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, asRealMember } from "./fixtures";
import { DEMO_IDS } from "../src/app/components/screens/demos/demo-catalog";

// M-6 sweep, real member token (asRealMember — a genuine seeded wdn_ token,
// never a spliced /me): the whole demo catalog, partitioned into the three
// buckets a member's Getting Started (member-getting-started.tsx) can put a
// demo in on this backend (real wardynd + Postgres + the `none` runner, no
// connected model, no demo secret stored):
//
//  - VISIBLE: walkable here (no needsModel/needsSecret precondition unmet)
//    AND not narrowed by the member's default governance ceiling
//    (setup/steps.ts's ceilingNarrows — owner ruling 2026-09-25: a demo the
//    ceiling would rewrite is HIDDEN from a member entirely, never offered
//    watch-only).
//  - NARROWED: walkable, but the ceiling hides it (allow_all_egress,
//    wait_for_review, or an api_key/git_pat/ssh_key/cloud_sts grant).
//  - UNWALKABLE: dropped for EVERY role on this backend (needsModel with no
//    connected model, or needsSecret with none of the five demo secrets
//    stored) — already the operator-side gap demos.spec.ts proves; listed
//    here only so the partition below accounts for the full catalog.
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

test.describe("member demos sweep (#685 M-6, real member token)", () => {
  test.beforeEach(async ({ page }) => {
    await asRealMember(page);
  });

  for (const id of VISIBLE) {
    test(`${id} reaches the member and offers Start`, async ({ page }) => {
      await page.goto(`/setup?step=${id}`);
      await expect(page.getByTestId(`demo-card-${id}`)).toBeVisible();
      // Same -runner-none fact demos.spec.ts pins for an operator: Start is
      // gated closed on this backend regardless of role.
      await expect(page.getByTestId(`demo-start-${id}`)).toBeVisible();
      await expect(page.getByTestId(`demo-start-${id}`)).toBeDisabled();
    });
  }

  for (const id of [...NARROWED, ...UNWALKABLE]) {
    test(`${id} never reaches a member's Getting Started`, async ({ page }) => {
      await page.goto(`/setup?step=${id}`);
      // A deep link this page doesn't offer opens nothing and errors
      // nothing (the same contract member-getting-started-demos.test.tsx
      // pins) — the page itself still renders.
      await expect(page.getByRole("heading", { name: "What's set up for you" })).toBeVisible();
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
});
