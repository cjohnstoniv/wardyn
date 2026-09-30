/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, gotoConsole, navTo } from "./fixtures";
import { AUTONOMY_META } from "../src/app/components/wardyn/autonomy-meta";
import {
  CHIP_IMAGE_PULL_FAILED,
  CHIP_SETTING_UP,
  CHIP_WAITING_FOR_MACHINE,
  PENDING_NO_DETAIL,
  STARTING_CONTAINER_CREATING,
  STUCK_IMAGE_PULL,
} from "../src/app/components/screens/run-status-detail";
import type { Page } from "@playwright/test";

// Split out of runs.spec.ts (#209): the run-detail command bar's own status
// chips (autonomy, the failure-hint/startup-reason chip, PENDING's queued
// sentence) against the seeded 9-fixture backend runs.spec.ts's own top
// comment maps out (fixture N -> state). Split by behaviour, not moved for
// size alone — this file's suites never mutate run state, unlike the
// kill/hold-timing suites that stayed in runs.spec.ts.
test.describe.configure({ mode: "serial" });

// Open the Runs screen and wait for the seeded page to render. PENDING
// fixture 0 is always live, so it always renders (as "Queued" now — #1197 D2
// folds PENDING into the Running section rather than a "Pending" badge).
async function openRuns(page: Page): Promise<void> {
  await gotoConsole(page);
  await navTo(page, "Runs");
  await expect(page.getByRole("heading", { name: "Runs", level: 1 })).toBeVisible();
  await expect(page.getByText("e2e fixture 0")).toBeVisible();
}

// #93/#97 — the run header's autonomy chip, beside ConfinementChip.
// run.autonomy_level freezes the level resolveRunAutonomy capped this run at,
// at create time — never populated by the seeded backend's operator bearer
// (same operator short-circuit governance.spec.ts's header note explains for
// the rail), so spliced onto the real GET response — the same
// route.fetch()+patch+refulfill technique the interactive/failure_hint splice
// in runs-detail.spec.ts uses.
test.describe("Run header — the autonomy chip (#93/#97)", () => {
  test("renders the level's friendly label beside the barrier chip", async ({ page }) => {
    await openRuns(page);
    await page.route("**/api/v1/runs/*", async (route) => {
      if (route.request().method() !== "GET") return route.fallback();
      const response = await route.fetch();
      const json = await response.json();
      if (json.task === "e2e fixture 2") json.autonomy_level = "L1";
      await route.fulfill({ response, json });
    });

    await page.getByText("e2e fixture 2").click();
    await expect(page).toHaveURL(/\/runs\/[0-9a-f-]{8,}/);
    const header = page.getByTestId("run-summary-header");
    await expect(header.getByText(AUTONOMY_META.L1.label, { exact: true })).toBeVisible();
    // The internal wire level stays out of accessible content (D4) — same
    // rule ConfinementChip's CC1/CC2/CC3 follows.
    await expect(header.getByText("L1", { exact: true })).toHaveCount(0);
  });

  test("an ordinary run (empty autonomy_level) renders no autonomy chip at all", async ({ page }) => {
    await openRuns(page);
    await page.getByText("e2e fixture 2").click();
    await expect(page).toHaveURL(/\/runs\/[0-9a-f-]{8,}/);
    const header = page.getByTestId("run-summary-header");
    for (const meta of Object.values(AUTONOMY_META)) {
      await expect(header.getByText(meta.label, { exact: true })).toHaveCount(0);
    }
  });
});

// #1234 — the thin "Launched via" line under the bar. The seeded backend has
// no registered portal, so created_via (and the name the server resolves for
// it, proven in internal/api/run_created_via_name_test.go) is spliced onto the
// real GET response, the same route.fetch()+patch+refulfill technique as above.
test.describe("Run header — Launched via (#1234)", () => {
  async function openFixture2(page: Page, splice?: (json: Record<string, unknown>) => void): Promise<void> {
    await openRuns(page);
    if (splice) {
      await page.route("**/api/v1/runs/*", async (route) => {
        if (route.request().method() !== "GET") return route.fallback();
        const response = await route.fetch();
        const json = await response.json();
        if (json.task === "e2e fixture 2") splice(json);
        await route.fulfill({ response, json });
      });
    }
    await page.getByText("e2e fixture 2").click();
    await expect(page).toHaveURL(/\/runs\/[0-9a-f-]{8,}/);
    await expect(page.getByTestId("run-summary-header")).toBeVisible();
  }

  test("a portal-launched run says which portal, under the bar", async ({ page }) => {
    await openFixture2(page, (json) => {
      json.created_via = "0b1c2d3e-0000-4000-8000-000000000001";
      json.created_via_name = "Acme Support Portal";
    });
    await expect(page.getByTestId("run-launched-via")).toHaveText("Launched via Acme Support Portal");
  });

  test("a portal the server could not name falls back to 'a portal'", async ({ page }) => {
    await openFixture2(page, (json) => {
      json.created_via = "0b1c2d3e-0000-4000-8000-000000000001";
    });
    await expect(page.getByTestId("run-launched-via")).toHaveText("Launched via a portal");
  });

  test("a self-launched run has no line", async ({ page }) => {
    await openFixture2(page);
    await expect(page.getByTestId("run-launched-via")).toHaveCount(0);
  });
});

// F1-F4 (verifier correction over the raised finding's own fix): `:164-172`
// documents the failure-hint chip as "the only place a FAILED run says why —
// stays visible at every width" — so the fix is NEVER hide it (min-w-0 shrink
// truncate), not the raised finding's `hidden … 2xl:inline-flex`. 420px is
// well below the bar's floor (~1300px on a single-line row before the fix),
// stressing the truncate/overflow-hidden path harder than the width loop in
// runs-detail.spec.ts.
test.describe("Run header — the failure-hint chip survives a narrow viewport", () => {
  // ticket: F1-F4
  // 0.7.6 finding 6: a STARTING run says what it is waiting ON. The seeded
  // backend has no real substrate behind fixture 1, so the reason is injected on
  // the read the console actually makes — the same route-intercept shape the
  // failure_hint case above uses.
  test("a STARTING run's header carries the substrate's reason, in the short register, with the sentence on its title", async ({
    page,
  }) => {
    await page.route("**/api/v1/runs/*", async (route) => {
      if (route.request().method() !== "GET") return route.fallback();
      const response = await route.fetch();
      const json = await response.json();
      if (json.task === "e2e fixture 1") {
        json.status_detail = "agent: ContainerCreating";
        json.status_reason = "ContainerCreating";
      }
      await route.fulfill({ response, json });
    });
    await openRuns(page);
    await page.getByText("e2e fixture 1").click();
    await expect(page).toHaveURL(/\/runs\/[0-9a-f-]{8,}/);

    const header = page.getByTestId("run-summary-header");
    await expect(header.getByText("Starting", { exact: true })).toBeVisible();
    // The SHORT register on screen, the sentence on the title: at max-w-[160px]
    // the sentence would truncate to a restatement of the badge beside it.
    await expect(header.getByText(CHIP_SETTING_UP)).toBeVisible();
    await expect(header.getByTitle(STARTING_CONTAINER_CREATING)).toBeVisible();
  });

  // The terminal arm, which is the one the 0.7.5 estate needed: the reason that
  // will not resolve, in the register that survives the chip's width.
  test("a terminal startup reason reads as a failure, not as progress", async ({ page }) => {
    await page.route("**/api/v1/runs/*", async (route) => {
      if (route.request().method() !== "GET") return route.fallback();
      const response = await route.fetch();
      const json = await response.json();
      if (json.task === "e2e fixture 1") {
        json.status_detail = "agent: ImagePullBackOff: rpc error: pull access denied";
        json.status_reason = "ImagePullBackOff";
      }
      await route.fulfill({ response, json });
    });
    await openRuns(page);
    await page.getByText("e2e fixture 1").click();
    await expect(page).toHaveURL(/\/runs\/[0-9a-f-]{8,}/);

    const header = page.getByTestId("run-summary-header");
    await expect(header.getByText(CHIP_IMAGE_PULL_FAILED)).toBeVisible();
    // The registry's own words are what name the fix, so they must survive to
    // the title even though the chip cannot hold them.
    await expect(header.getByTitle(new RegExp(`${STUCK_IMAGE_PULL} .*pull access denied`))).toBeVisible();
  });

  // #725/F3 — the header's status chip is max-w-[160px] shrink truncate
  // exactly so a long sentence can never force the box wider than its own
  // box (the sentence itself rides only the `title`, where width is free).
  // An unrecognised reason token falls through statusDetailChip's default
  // arm ("Waiting: <reason>") with no length limit of its own — the one
  // arm that can still carry an arbitrarily long string into the chip.
  test("an unrecognised, long startup reason never widens the chip past its own box", async ({ page }) => {
    const longReason = "SomeVeryLongUnrecognisedSubstrateReasonTokenNeverSeenBeforeInThisRegistry";
    await page.route("**/api/v1/runs/*", async (route) => {
      if (route.request().method() !== "GET") return route.fallback();
      const response = await route.fetch();
      const json = await response.json();
      if (json.task === "e2e fixture 1") {
        json.status_detail = `agent: ${longReason}`;
        json.status_reason = longReason;
      }
      await route.fulfill({ response, json });
    });
    await openRuns(page);
    await page.getByText("e2e fixture 1").click();
    await expect(page).toHaveURL(/\/runs\/.+/);

    const header = page.getByTestId("run-summary-header");
    // The chip's visible text is the SHORT register (statusDetailChip: just
    // the reason token, "Waiting: <reason>") — its `title` carries the full
    // SENTENCE register instead (statusDetailSentence: the raw "component:
    // reason" line), which is why the two differ here.
    await expect(header.getByText(`Waiting: ${longReason}`, { exact: true })).toBeVisible();
    const chip = header.getByTitle(`Waiting: agent: ${longReason}`);
    await expect(chip).toBeVisible();
    const overflow = await chip.evaluate((el) => ({ scrollWidth: el.scrollWidth, clientWidth: el.clientWidth }));
    expect(overflow.scrollWidth, "status chip scrollWidth").toBeLessThanOrEqual(overflow.clientWidth);
    // The box itself stays capped too: without max-w-[160px] the chip grows to
    // fit the whole reason and the overflow check above still passes.
    const chipBox = await chip.boundingBox();
    expect(chipBox, "status chip boundingBox").not.toBeNull();
    expect(chipBox!.width, "status chip width").toBeLessThanOrEqual(160);
  });

  test("no horizontal overflow at 420px, Kill stays in the viewport, and the hint chip is still visible", async ({
    page,
  }) => {
    await openRuns(page);
    const hint = "container exited with code 137: OOMKilled while installing dependencies";
    await page.route("**/api/v1/runs/*", async (route) => {
      if (route.request().method() !== "GET") return route.fallback();
      const response = await route.fetch();
      const json = await response.json();
      if (json.task === "e2e fixture 6") json.failure_hint = hint;
      await route.fulfill({ response, json });
    });

    await page.getByText("e2e fixture 6").click();
    await expect(page).toHaveURL(/\/runs\/[0-9a-f-]{8,}/);
    await expect(page.getByText("Failed", { exact: true })).toBeVisible();

    await page.setViewportSize({ width: 420, height: 720 });
    // Settled first: measured before the tab strip mounts, the page is still
    // the reload skeleton and the overflow check below passes vacuously.
    await expect(page.getByRole("tab", { name: "Recording" })).toBeVisible();

    // THE PAGE DOES NOT SCROLL (run-detail.tsx's own invariant) — a residual
    // overflow here would force <main>'s overflow-y:auto into overflow-x too.
    // Polled: the viewport change re-lays the page out (and the header's
    // responsive variants re-render) after setViewportSize returns, so a
    // single read can catch the wide layout mid-transition.
    await expect
      .poll(
        () =>
          page.evaluate(() => {
            const main = document.querySelector("main");
            return (main?.scrollWidth ?? 0) - (main?.clientWidth ?? 0);
          }),
        { message: "main scrollWidth minus clientWidth at 420px" },
      )
      .toBe(0);

    const killBtn = page.getByRole("button", { name: "Kill", exact: true });
    await expect(killBtn).toBeVisible();
    const killBox = await killBtn.boundingBox();
    expect(killBox, "Kill boundingBox at 420px").not.toBeNull();
    expect(killBox!.x + killBox!.width, "Kill right edge at 420px").toBeLessThanOrEqual(420);

    // NEVER hidden — the chip may truncate, but it must still be on screen.
    await expect(page.getByTitle(hint)).toBeVisible();
  });

  // The five-tab strip (Overview, Approvals, Policy, Audit, Recording) is
  // wider than 390px: it must scroll in place, never widen <main>, and the
  // last tab must stay reachable.
  test("the tab strip scrolls in place at 390px and the Recording tab stays reachable", async ({ page }) => {
    await openRuns(page);
    await page.getByText("e2e fixture 6").click();
    await expect(page).toHaveURL(/\/runs\/[0-9a-f-]{8,}/);
    await expect(page.getByText("Failed", { exact: true })).toBeVisible();

    await page.setViewportSize({ width: 390, height: 720 });
    const recording = page.getByRole("tab", { name: "Recording" });
    await expect(recording).toBeVisible();

    await expect
      .poll(
        () =>
          page.evaluate(() => {
            const main = document.querySelector("main");
            return (main?.scrollWidth ?? 0) - (main?.clientWidth ?? 0);
          }),
        { message: "main scrollWidth minus clientWidth at 390px" },
      )
      .toBe(0);

    await recording.scrollIntoViewIfNeeded();
    const box = await recording.boundingBox();
    expect(box, "Recording tab boundingBox at 390px").not.toBeNull();
    expect(box!.x + box!.width, "Recording tab right edge at 390px").toBeLessThanOrEqual(390);
  });
});

// #125 — PENDING's own first tick, before the substrate has sent anything at
// all: statusChip widens from STARTING-only to STARTING || PENDING, and an
// empty status_detail on a PENDING run gets PENDING_NO_DETAIL instead of
// rendering nothing. Route-spliced on fixture 0 (seeded PENDING) the same
// shape the STARTING cases above use — anchored on the run's own id
// (**/api/v1/runs/*, a single path segment), never a bare `/\/runs\/.+/`
// against the page URL, which also matches /runs/new.
test.describe("Run header — PENDING's own queued sentence (#125)", () => {
  test("shows the queued sentence with no status_detail, and a real stage line replaces it", async ({ page }) => {
    let stage: { status_detail: string; status_reason: string } | null = null;
    await page.route("**/api/v1/runs/*", async (route) => {
      if (route.request().method() !== "GET") return route.fallback();
      const response = await route.fetch();
      const json = await response.json();
      if (json.task === "e2e fixture 0") {
        json.status_detail = stage?.status_detail ?? "";
        json.status_reason = stage?.status_reason ?? "";
      }
      await route.fulfill({ response, json });
    });
    await openRuns(page);
    await page.getByText("e2e fixture 0").click();
    await expect(page).toHaveURL(/\/runs\/[0-9a-f-]{8,}/);

    const header = page.getByTestId("run-summary-header");
    await expect(header.getByText("Pending", { exact: true })).toBeVisible();
    // SF-25: the mock (packet-4.html state 3) has only the reused Pending
    // badge plus this sentence as a visible line — no separate "Queued" info
    // chip, which would say the badge's own fact a second time.
    await expect(page.getByText(PENDING_NO_DETAIL, { exact: true })).toBeVisible();
    await expect(header.getByText("Queued", { exact: true })).toHaveCount(0);

    // The next poll tick (DETAIL_POLL_MS) picks up a real stage line, which
    // supersedes the queued sentence.
    stage = { status_detail: "pod: Unschedulable: no room", status_reason: "Unschedulable" };
    await expect(header.getByText(CHIP_WAITING_FOR_MACHINE)).toBeVisible({ timeout: 8_000 });
    await expect(page.getByText(PENDING_NO_DETAIL, { exact: true })).toHaveCount(0);
  });
});
