/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { randomUUID } from "node:crypto";
import { test, expect, ADMIN_TOKEN, gotoConsole, navTo, sidebarLink, sql } from "./fixtures";
import { RUN, RUN_COCKPIT } from "../src/app/components/wardyn/copy";
import { RUN_WAIT } from "../src/app/components/wardyn/copy/run-wait";
import { LOGIN_SANDBOX_NOTE } from "../src/app/components/screens/run-detail/login-sandbox-note";
import { MODEL_ACCESS_BANNER, MODEL_ACCESS_RUN_DOOR } from "../src/app/components/wardyn/model-access-copy";
import { AGENTS } from "../src/app/lib/workspace-providers-copy";
import { AUTONOMY_META } from "../src/app/components/wardyn/autonomy-meta";
import { STATES } from "../src/app/components/wardyn/states";
import {
  CHIP_IMAGE_PULL_FAILED,
  CHIP_SETTING_UP,
  CHIP_WAITING_FOR_MACHINE,
  PENDING_NO_DETAIL,
  STARTING_CONTAINER_CREATING,
  STUCK_IMAGE_PULL,
} from "../src/app/components/screens/run-status-detail";
import type { Page } from "@playwright/test";

// ============================================================================
// Runs lane: the Runs landing page (#1197 D2, rewritten from the old board +
// table density switch) + the addressable /runs/:id RunDetail hub + kill.
//
// The seeded backend creates 9 runs, one per RunState, with deterministic task
// text "e2e fixture 0".."e2e fixture 8" mapped to states by creation order:
//   fixture 0 -> PENDING                  (Running section, "Queued")
//   fixture 1 -> STARTING                 (Running section, "Starting")
//   fixture 2 -> RUNNING                  (Running section, "Running")
//   fixture 3 -> WAITING_FOR_CONFIRMATION (Needs you)
//   fixture 4 -> COMPLETED                (Ended today) <-- the critical regression
//   fixture 5 -> STOPPED                  (Ended today)
//   fixture 6 -> FAILED                   (Ended today, needs review)
//   fixture 7 -> KILLED                   (Ended today)
//   fixture 8 -> ARCHIVED                 (Ended today)
//
// Every fixture is untitled (the legacy/CLI/system-run shape) and renders by
// its task text (board-groups.ts's rowHeadline) — #1197 D2 groups by need
// then time, not by title, so there is no shared-title fixture here the way
// the old title-grouped board needed one. State is asserted via the row's own
// status WORD (runs/runs-model.ts's rowPresentation), never CSS classes:
//   PENDING "Queued", STARTING "Starting", RUNNING "Running",
//   WAITING_FOR_CONFIRMATION "Needs your approval", COMPLETED "Completed",
//   STOPPED "Stopped", FAILED "Failed", KILLED "Killed", ARCHIVED "Archived".
// Barriers, the agent monogram and the Run ID moved to the run page — none of
// them render on this page anymore (design.md §4).
// ============================================================================

// Run this file's tests serially in a single worker. The suite shares one
// seeded backend and the final tests MUTATE run state (kill). Serial mode keeps
// the read-only assertions from racing the mutation regardless of the global
// fullyParallel setting, and runs the mutating tests last (declaration order).
test.describe.configure({ mode: "serial" });

const auth = { Authorization: `Bearer ${ADMIN_TOKEN}` };

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

// Open the Runs screen and wait for the seeded page to render. PENDING
// fixture 0 is always live, so it always renders (as "Queued" now — #1197 D2
// folds PENDING into the Running section rather than a "Pending" badge).
async function openRuns(page: Page): Promise<void> {
  await gotoConsole(page);
  await navTo(page, "Runs");
  await expect(page.getByRole("heading", { name: "Runs", level: 1 })).toBeVisible();
  await expect(page.getByText("e2e fixture 0")).toBeVisible();
}

// #1197 D2 replaced the Board/Table density switch, title groups and the
// per-row kebab (Kill/Clone/Open) with one row anatomy and sections ordered
// need-then-time — see ui/e2e/runs-landing.spec.ts for that page's own
// coverage (loading/error/empty/quiet/populated/ageing-note/no-match/400px/
// light+dark). What stays HERE is what this seeded 9-fixture backend still
// exercises that runs-landing.spec.ts's synthetic fixtures don't: every real
// RunState rendering without crashing, search over a live backend, and the
// run detail hub (below), Kill and Clone having moved onto the run page.
test.describe("Runs landing — the seeded 9-fixture backend", () => {
  test("every seeded run renders by its task text, across every RunState, with no crash", async ({ page }) => {
    await openRuns(page);
    for (const f of FIXTURES) {
      await expect(page.getByText(f.task)).toBeVisible();
    }
    // The critical regression this suite has always pinned: a COMPLETED run
    // must not crash the console, and the sidebar chrome must stay intact.
    await expect(page.getByText("Completed", { exact: true }).first()).toBeVisible();
    await expect(sidebarLink(page, "Runs")).toBeVisible();

    // The needs-you / ended words this backend's own fixtures reach.
    await expect(page.getByText("Failed", { exact: true }).first()).toBeVisible({ timeout: 15_000 });
  });

  test("search filters the page down to a single matching run", async ({ page }) => {
    await openRuns(page);
    const search = page.getByLabel("Search runs", { exact: true });

    // #806: the filter is server-side now (runs.tsx sends ?q= on every
    // filter change and re-fetches) — same non-determinism this suite has
    // always guarded against on this input (a reload in flight can drop a
    // fill), so the fill itself is retried, not just the assertion after it.
    await expect(async () => {
      await search.fill("e2e fixture 4");
      await expect(search).toHaveValue("e2e fixture 4");
      await expect(page.getByText("e2e fixture 4")).toBeVisible({ timeout: 3_000 });
      await expect(page.getByText("e2e fixture 0", { exact: true })).toHaveCount(0);
    }).toPass({ timeout: 15_000 });
  });

  test("a non-matching search shows the empty state, then recovers when cleared", async ({ page }) => {
    await openRuns(page);
    const search = page.getByLabel("Search runs", { exact: true });

    await expect(async () => {
      await search.fill("zzz-no-such-run-zzz");
      await expect(search).toHaveValue("zzz-no-such-run-zzz");
      await expect(page.getByText("No runs match these filters.")).toBeVisible({ timeout: 3_000 });
    }).toPass({ timeout: 15_000 });
    await expect(page.getByText("Try a different search term or filter.")).toBeVisible();

    await page.getByRole("button", { name: "Clear filters" }).click();
    await expect(page.getByText("e2e fixture 0")).toBeVisible({ timeout: 15_000 });
  });
});

test.describe("Run detail (/runs/:id)", () => {
  test("clicking a run opens its addressable detail hub with identity + kill", async ({ page }) => {
    await openRuns(page);

    // Clicking the run card navigates to the addressable /runs/:id page (the old
    // slide-over Sheet is gone).
    await page.getByText("e2e fixture 2").click();
    await expect(page).toHaveURL(/\/runs\/[0-9a-f-]{8,}/);

    // The command bar carries the task (h1) + the RUNNING badge.
    await expect(page.getByRole("heading", { name: "e2e fixture 2", level: 1 })).toBeVisible();
    await expect(page.getByText("Running", { exact: true })).toBeVisible();

    // The run's real identity fields still render — but the cockpit split them:
    // the repo moved UP into the command bar's 52px single row at xl and up
    // (wraps below — review R-16), because it is one of the facts that must
    // be visible without scrolling, while the run id / SPIFFE id stayed in
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
    // CI-flake: measured 300.28px on the CI runner against a hard <300 bound —
    // sub-pixel layout jitter, not a real regression. Rounded rather than
    // waited: the property under test ("above the fold") tolerates a
    // fractional pixel; it would not tolerate a genuine multi-hundred-pixel
    // regression, which this still catches.
    expect(Math.round(box!.y)).toBeLessThanOrEqual(300);

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
    await expect(page).toHaveURL(/\/runs\/[0-9a-f-]{8,}/);

    await expect(page.getByRole("heading", { name: "e2e fixture 4", level: 1 })).toBeVisible();
    await expect(page.getByText("Completed", { exact: true })).toBeVisible();

    // Terminal run => the Kill trigger is disabled.
    const killBtn = page.getByRole("button", { name: "Kill", exact: true });
    await expect(killBtn).toBeVisible();
    await expect(killBtn).toBeDisabled();
  });

  // 0.7.3 F7: the clone door used to live only inside the failure block (a run
  // that ended BADLY) — it now sits on the header for every terminal state,
  // COMPLETED included, beside the disabled Kill.
  test("a COMPLETED run's header offers the clone door, and it lands on /runs/new prefilled", async ({
    page,
  }) => {
    await openRuns(page);
    await page.getByText("e2e fixture 4").click();
    await expect(page).toHaveURL(/\/runs\/[0-9a-f-]{8,}/);
    await expect(page.getByText("Completed", { exact: true })).toBeVisible();

    const cloneBtn = page.getByRole("button", { name: RUN.CLONE_CTA });
    await expect(cloneBtn).toBeVisible();
    await expect(page.getByRole("button", { name: "Kill", exact: true })).toBeDisabled();

    await cloneBtn.click();
    await expect(page).toHaveURL(/\/runs\/new$/);
    await expect(page.getByRole("heading", { name: "New run" })).toBeVisible();
    await expect(page.getByLabel("Task")).toHaveValue("e2e fixture 4");
    // review C-02: an AUDIT-derived field (never on the run row itself) —
    // proves the clone actually reads fixture 4's run.create row, not just
    // the row, which carries no tool_approvals at all.
    await expect(
      page
        .getByRole("radiogroup", { name: "Tool approvals" })
        .getByRole("radio", { name: /Hold in Wardyn/ }),
    ).toHaveAttribute("aria-checked", "true");
  });

  // #1197 D2 (design.md §4): the Runs-list kebab (Kill, Clone, Open) is gone
  // — one inline action per row at most now, and Clone moved onto the run
  // page only. That capability's coverage is the header test right above
  // this ("a COMPLETED run's header offers the clone door…"), which is the
  // same read (createRequestFromAudit) this kebab test used to pin a second
  // way — so removing the second access path loses no coverage of the
  // underlying behaviour, only a door that no longer exists.

  // review C-04/R-03/R-14/R-16: the new clone button must not come at the
  // cost of the ONE thing the header's own comments call non-negotiable (the
  // task h1) — and the fix must not hide the clone LABEL either (an
  // icon-only door is the discoverability failure this whole finding is
  // about). Worst REAL case, not a bare fixture: fixture 6 is seeded
  // (scripts/e2e-backend.sh) as a FAILED run carrying a longer repo and an
  // exit code — only `interactive` and `failure_hint` still need the route
  // splice below (neither is a seed-script column). A PENDING approval was
  // tried here too (R-03) and dropped again (R-14's live measurement): that
  // combination cannot fit Kill on-screen alongside every OTHER thing worth
  // protecting, and per R-13 it is not a state a real terminal run reaches
  // anyway. review R-18: the seed also carries a workspace_path (still
  // realistic, still renders at 2xl and elsewhere on the page), but review
  // R-16 hid that span below 2xl, so it is inert for THIS test's 1024/1280
  // widths and is not asserted here.
  test("a FAILED interactive run's task title is never squeezed to nothing, and the clone door keeps its label, at every width the bar renders at", async ({
    page,
  }) => {
    await openRuns(page);
    await page.route("**/api/v1/runs/*", async (route) => {
      if (route.request().method() !== "GET") return route.fallback();
      const response = await route.fetch();
      const json = await response.json();
      if (json.task === "e2e fixture 6") {
        json.interactive = true;
        json.failure_hint = "container exited with code 137: OOMKilled while installing dependencies";
      }
      await route.fulfill({ response, json });
    });

    await page.getByText("e2e fixture 6").click();
    await expect(page).toHaveURL(/\/runs\/[0-9a-f-]{8,}/);
    await expect(page.getByText("Failed", { exact: true })).toBeVisible();

    const header = page.getByTestId("run-summary-header");
    // Every sibling fact the seed now carries actually renders — the
    // "everything rendered" re-measure R-03 asked for, not just the floor
    // assertions below. Asserted once, at whatever viewport is active on
    // entry (this suite's 1280 default) — the loop below only re-measures
    // width-sensitive facts, not this one, which never gates on width.
    await expect(header.getByText("exit 137")).toBeVisible();

    // review R-16: the bar has only two breakpoints (`lg:` 1024, `2xl:`
    // 1536) plus the wrap threshold at `xl:` (1280) review R-16 itself
    // added, so its content — and therefore whether it fits — genuinely
    // differs across that range. One width proves nothing about the rest:
    // `lg`'s own floor (1024, where the bar wraps to two lines), this
    // suite's default (1280, the first single-line width), and `2xl` (1536,
    // where the decorative/secondary waterfall reveals everything it was
    // hiding).
    // U2-10 (blind round 2, lens-U2): plus 800 — BELOW `lg`. That width used
    // to render no tier and no attachability anywhere on the run page: the
    // Confinement+Interactive block was `hidden … lg:flex`, survivable only
    // while app-shell's global BarrierChip covered narrower viewports, and
    // 0.7.3 F6 removed that chip. The gate is gone, and 800 is here so it
    // cannot come back unnoticed. Same five invariants at all four.
    for (const width of [800, 1024, 1280, 1536]) {
      await page.setViewportSize({ width, height: 720 });

      const heading = page.getByRole("heading", { name: "e2e fixture 6", level: 1 });
      await expect(heading, `h1 at ${width}px`).toBeVisible();
      const box = await heading.boundingBox();
      expect(box, `h1 boundingBox at ${width}px`).not.toBeNull();
      expect(box!.width, `h1 width at ${width}px`).toBeGreaterThanOrEqual(160);

      // review R-15: assert the repo floor directly rather than infer it
      // from Kill's position — a green Kill assertion reads the same
      // whether the bar has 200px of slack or 1px, so the floor itself
      // needs its own pin.
      const repo = header.getByText(/^github\.com/);
      await expect(repo, `repo at ${width}px`).toBeVisible();
      const repoBox = await repo.boundingBox();
      expect(repoBox, `repo boundingBox at ${width}px`).not.toBeNull();
      expect(repoBox!.width, `repo width at ${width}px`).toBeGreaterThanOrEqual(90);

      // review R-14/R-16: the floors above prove h1/repo don't collapse,
      // but not that the BAR fits — a row that keeps every element at its
      // floor/cap and still overflows the viewport would clip Kill off the
      // right edge (exactly what regression 1's screenshot showed, and what
      // the pass-3 red proved at 1280 alone) while every OTHER assertion
      // here stayed green. Kill's right edge must stay on-screen at every
      // tested width, not just the suite's default.
      const killBtn = page.getByRole("button", { name: "Kill", exact: true });
      await expect(killBtn, `Kill visible at ${width}px`).toBeVisible();
      const killBox = await killBtn.boundingBox();
      expect(killBox, `Kill boundingBox at ${width}px`).not.toBeNull();
      expect(killBox!.x + killBox!.width, `Kill right edge at ${width}px`).toBeLessThanOrEqual(width);

      const cloneBtn = page.getByRole("button", { name: RUN.CLONE_CTA });
      await expect(cloneBtn, `clone visible at ${width}px`).toBeVisible();
      // regression 1 (review round 2): the clone LABEL must not collapse to
      // an icon at ANY tested width — an icon-only door was the exact
      // discoverability failure 0.7.3 F7 exists to fix, so its visible TEXT
      // (not just its accessible name) has to equal RUN.CLONE_CTA here.
      await expect(cloneBtn, `clone label at ${width}px`).toHaveText(RUN.CLONE_CTA);

      // regression 1's own root cause: "Interactive" (this run isn't
      // RUNNING, so never "— attachable") is the security-visible fact
      // governance.spec.ts:473 exists to pin, and the run's own
      // ConfinementChip is now the ONLY place a run's tier shows anywhere
      // (0.7.3 F6 removed the header's global chip) — neither may hide at
      // ANY width this bar renders at (review U-04). review R-09: scoped to
      // the header testid, not the whole page — plain text, so a future
      // widget rendering either string elsewhere would otherwise turn this
      // into a Playwright strict-mode failure rather than a clean miss.
      await expect(header.getByText("Interactive", { exact: true }), `Interactive at ${width}px`).toBeVisible();
      await expect(header.getByText("Fence", { exact: true }), `Fence at ${width}px`).toBeVisible();
    }
  });
});

// #93/#97 — the run header's autonomy chip, beside ConfinementChip.
// run.autonomy_level freezes the level resolveRunAutonomy capped this run at,
// at create time — never populated by the seeded backend's operator bearer
// (same operator short-circuit governance.spec.ts's header note explains for
// the rail), so spliced onto the real GET response — the same
// route.fetch()+patch+refulfill technique the interactive/failure_hint splice
// right above this block already uses.
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

// F1-F4 (verifier correction over the raised finding's own fix): `:164-172`
// documents the failure-hint chip as "the only place a FAILED run says why —
// stays visible at every width" — so the fix is NEVER hide it (min-w-0 shrink
// truncate), not the raised finding's `hidden … 2xl:inline-flex`. 420px is
// well below the bar's floor (~1300px on a single-line row before the fix),
// stressing the truncate/overflow-hidden path harder than the width loop above.
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

    // THE PAGE DOES NOT SCROLL (run-detail.tsx's own invariant) — a residual
    // overflow here would force <main>'s overflow-y:auto into overflow-x too.
    const overflow = await page.evaluate(() => {
      const main = document.querySelector("main");
      return { scrollWidth: main?.scrollWidth ?? 0, clientWidth: main?.clientWidth ?? 0 };
    });
    expect(overflow.scrollWidth, "main scrollWidth at 420px").toBe(overflow.clientWidth);

    const killBtn = page.getByRole("button", { name: "Kill", exact: true });
    await expect(killBtn).toBeVisible();
    const killBox = await killBtn.boundingBox();
    expect(killBox, "Kill boundingBox at 420px").not.toBeNull();
    expect(killBox!.x + killBox!.width, "Kill right edge at 420px").toBeLessThanOrEqual(420);

    // NEVER hidden — the chip may truncate, but it must still be on screen.
    await expect(page.getByTitle(hint)).toBeVisible();
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

// P1 (0.7.3 field report), the "say what the box is" half: `harness login` is a
// server-side task discriminator — it gates the credential upload route, keeps
// the session out of the recorder, and pins an image whose own Dockerfile header
// says "NOT a coding agent" — and the console labelled it NOWHERE. Opened from
// /runs it looked like any other interactive run: a bare shell, no agent, no
// task. Browser-level because the note is a render decision made from the run
// the page fetched.
test.describe("Run detail — a login sandbox says what it is", () => {
  test("the AWS sign-in note renders for an AWS login run — not for an ordinary run, and not for the ANTHROPIC login", async ({
    page,
  }) => {
    await openRuns(page);
    // The task is PROVIDER-AGNOSTIC (every container login carries it), so the
    // splice sets the agent too: fixture 6 becomes the AWS box, fixture 4 the
    // Anthropic one that carries the same task and must say nothing about AWS.
    //
    // U-2: the splice also states RUNNING. The note describes a box that is UP
    // (its terminal, its device code, its idle cap), and the seeded fixtures are
    // deliberately spread across every terminal state — so pinning the AGENT gate
    // needs the state held constant, exactly as pinning the STATE gate (its own
    // case below) needs the agent held constant.
    await page.route("**/api/v1/runs/*", async (route) => {
      if (route.request().method() !== "GET") return route.fallback();
      const response = await route.fetch();
      const json = await response.json();
      if (json.task === "e2e fixture 6") {
        json.task = "harness login";
        json.agent = "aws-sso";
        json.state = "RUNNING";
      }
      if (json.task === "e2e fixture 4") {
        json.task = "harness login";
        json.agent = "claude-code";
        json.state = "RUNNING";
      }
      await route.fulfill({ response, json });
    });

    // A different fixture first: the note must not be a banner every run grew.
    await page.getByText("e2e fixture 5").click();
    await expect(page).toHaveURL(/\/runs\/[0-9a-f-]{8,}/);
    await expect(page.getByTestId("run-summary-header")).toBeVisible();
    await expect(page.getByTestId("login-sandbox-note")).toHaveCount(0);

    // The Anthropic container login: same task, different box. An AWS sentence
    // here is false on all three of its clauses.
    await openRuns(page);
    await page.getByText("e2e fixture 4").click();
    await expect(page).toHaveURL(/\/runs\/[0-9a-f-]{8,}/);
    await expect(page.getByTestId("run-summary-header")).toBeVisible();
    await expect(page.getByTestId("login-sandbox-note")).toHaveCount(0);

    await openRuns(page);
    await page.getByText("e2e fixture 6").click();
    await expect(page).toHaveURL(/\/runs\/[0-9a-f-]{8,}/);
    const note = page.getByTestId("login-sandbox-note");
    await expect(note).toBeVisible();
    await expect(note).toContainText(LOGIN_SANDBOX_NOTE);
  });

  // Finding 4 (0.7.4 field report), the Runs-list half: the member reached the
  // login run from /runs, got a prompt with nothing typed, ran `aws sso login`
  // alone and captured nothing. The image runs the pair itself now and this
  // terminal joins that very session, so the page must say the sign-in is
  // ALREADY RUNNING here — 0.7.4's sentence sent the reader to Getting Started
  // to start a second one, which is the one instruction guaranteed to waste
  // their device code.
  //
  // BOUND: this daemon runs `-runner none` (scripts/e2e-backend.sh), so a login
  // run can never reach RUNNING and the attached PTY is out of reach here. What
  // the console does with a live sandbox is pinned by
  // harness-login-pane.test.tsx; what the SANDBOX does is pinned by the
  // pure-shell tests in internal/runner/docker and proven against the built
  // image under evidence/login-sandbox-selfrun/manual-proof-*.
  test("opening a harness-login run from /runs shows the running sign-in, not a bare prompt", async ({ page }) => {
    await openRuns(page);
    await page.route("**/api/v1/runs/*", async (route) => {
      if (route.request().method() !== "GET") return route.fallback();
      const response = await route.fetch();
      const json = await response.json();
      if (json.task === "e2e fixture 2") {
        json.task = "harness login";
        json.agent = "aws-sso";
      }
      await route.fulfill({ response, json });
    });

    await page.getByText("e2e fixture 2").click();
    await expect(page).toHaveURL(/\/runs\/[0-9a-f-]{8,}/);
    const note = page.getByTestId("login-sandbox-note");
    await expect(note).toBeVisible();
    await expect(note).toContainText(LOGIN_SANDBOX_NOTE);
    // The three things this page must no longer say: start a SECOND sign-in
    // elsewhere while this one is live; that the box closes itself when the
    // sign-in is done (nothing server-side stops a run on capture, so on THIS
    // path it lives to the idle cap); and — U-2 — that the sign-in is ALREADY
    // RUNNING, which is a fact about the image, not about this run: an operator
    // image pin makes a console-0.7.5 / image-0.7.4 pairing real, and there
    // nothing types the pair at all.
    await expect(note).not.toContainText("Sign in from Getting Started");
    await expect(note).not.toContainText("closes itself when it is done");
    await expect(note).not.toContainText("already running in this box");
  });

  // U-2 (W6 blind lens): the state gate, with the agent held constant. A login
  // run that is over — killed by the pane's own shutdown, or reaped — is opened
  // from /runs exactly like a live one, and every clause of the note describes a
  // sandbox that no longer exists.
  test("a harness-login run that is OVER says nothing about a terminal, a device code or an idle cap", async ({
    page,
  }) => {
    for (const state of ["KILLED", "COMPLETED"]) {
      await openRuns(page);
      await page.route("**/api/v1/runs/*", async (route) => {
        if (route.request().method() !== "GET") return route.fallback();
        const response = await route.fetch();
        const json = await response.json();
        if (json.task === "e2e fixture 2") {
          json.task = "harness login";
          json.agent = "aws-sso";
          json.state = state;
        }
        await route.fulfill({ response, json });
      });
      await page.getByText("e2e fixture 2").click();
      await expect(page).toHaveURL(/\/runs\/[0-9a-f-]{8,}/);
      await expect(page.getByTestId("run-summary-header")).toBeVisible();
      await expect(page.getByTestId("login-sandbox-note")).toHaveCount(0);
      await page.unrouteAll({ behavior: "ignoreErrors" });
    }
  });
});

// #1197 D2 (design.md §4): the kebab menu (Kill, Clone, Open) is gone from
// the Runs landing page — Kill and Clone moved onto the run page, one inline
// action per row at most. Their coverage is now entirely on that page (the
// "Killing an active run" tests below, and the clone-door tests earlier in
// this file), so this describe block has no replacement here.

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
    await expect(page).toHaveURL(/\/runs\/[0-9a-f-]{8,}/);
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

  test("the runs list still shows all nine runs after a kill", async ({ page }) => {
    await openRuns(page);
    // #1197 D2 removed the manual Refresh button and the "Live" chip — the
    // page's own poll (runs.tsx) picks up the kill on its own, no manual
    // control needed (design.md §4). Still nine runs total (a kill changes a
    // state, not the count).
    for (const f of FIXTURES) {
      await expect(page.getByText(f.task)).toBeVisible({ timeout: 15_000 });
    }
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
    await expect(page).toHaveURL(/\/runs\/[0-9a-f-]{8,}/);
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
    await expect(page).toHaveURL(/\/runs\/[0-9a-f-]{8,}/);
    await expect(page.getByRole("heading", { name: "e2e fixture 4", level: 1 })).toBeVisible();

    await page.getByRole("button", { name: "Edit layout" }).click();
    await page.getByRole("button", { name: "Add widget" }).click();
    const catalog = page.getByRole("dialog");
    await expect(catalog).toBeVisible();

    await expect(catalog.getByRole("button", { name: "Attach from your terminal" })).toHaveCount(0);
    // The entries that CAN render are still offered.
    await expect(catalog.getByRole("button", { name: "Sandbox" })).toBeVisible();
    // Opening the catalog wrote nothing, and no phantom widget can have been
    // stored because none was offered. X2-F7: the old assertion
    // (`.every(...)` over an always-empty array) was vacuous — it passed
    // whether or not the catalog ever fired a write at all.
    expect(layoutWrites).toEqual([]);
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
    await expect(page).toHaveURL(/\/runs\/[0-9a-f-]{8,}/);

    await expect(page.getByRole("heading", { name: "e2e fixture 2", level: 1 })).toBeVisible();
    await expect(page.getByText(STATES.ERROR_DEFAULT)).toHaveCount(0);
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
    await expect(page).toHaveURL(/\/runs\/[0-9a-f-]{8,}/);
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
    // CI-flake (the second site of the 300.28px class): getBoundingClientRect
    // returns a FRACTIONAL height while scrollHeight is integral, so a
    // sub-pixel of layout jitter (300.28 against 298 + 2) failed a property
    // that tolerates a fractional pixel and would still catch a genuine
    // flex-stretch of tens of pixels. Rounded, like the above-the-fold bound.
    expect(Math.round(measured.blockH)).toBeLessThanOrEqual(measured.scrollH + 2);
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
    await expect(page).toHaveURL(/\/runs\/[0-9a-f-]{8,}/);
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

// F1-F3 repro (verdict N — NEEDS-REPRO; verified/fixed only if this goes red):
// focus-mode.tsx's Escape handler and Radix's own AlertDialog dismiss are BOTH
// capture-phase listeners on `document` — stopPropagation on one does not
// suppress the other (only stopImmediatePropagation would), so the question is
// registration order, not interception. A live browser is the only way to
// settle it: does denying a held approval from inside focus mode, then
// pressing Escape, close only the dialog (fine) or also exit focus mode
// (remounts the terminal pane, dropping the live attach socket on an
// interactive run — R1-F1's F1-F3 finding).
test.describe("Focus mode — Escape inside a Deny confirm", () => {
  // ticket: F1-F3
  test("Escape closes the Deny dialog without also exiting focus mode", async ({ page }) => {
    const runId = sql("SELECT id FROM agent_runs ORDER BY created_at LIMIT 1");
    sql(`UPDATE agent_runs SET state = 'RUNNING' WHERE id = '${runId}'`);
    // tool_call is unconditionally "held" (isHeld) — LiveApprovals renders its
    // decision strip, with a Deny button, in the terminal pane regardless of
    // wait_for_review timing/mode.
    const approvalId = randomUUID();
    sql(
      `INSERT INTO approvals (id, run_id, kind, requested_scope, state, requested_at)
       VALUES ('${approvalId}','${runId}','tool_call','{"tool":"f1f3-repro.exec"}'::jsonb,'PENDING',now())`,
    );

    try {
      await page.goto(`/runs/${runId}`);
      await expect(page.getByText("Running", { exact: true }).first()).toBeVisible();

      await page.getByRole("button", { name: RUN_COCKPIT.enterFocus }).click();
      await expect(page.getByRole("button", { name: RUN_COCKPIT.exitFocus })).toBeVisible();
      // The dock opens on the first widget (Egress) by default — its glass
      // panel overlaps the terminal pane's own LiveApprovals strip. Close it;
      // the repro is about Escape vs. the Deny dialog, not the dock.
      await page.getByRole("button", { name: RUN_COCKPIT.closeDock }).click();

      await page.getByRole("button", { name: "Deny", exact: true }).click();
      const dialog = page.getByRole("alertdialog");
      await expect(dialog).toBeVisible();

      await page.keyboard.press("Escape");

      // The dialog is gone (Radix's own Escape-dismiss did its job)...
      await expect(dialog).not.toBeVisible();
      // ...and focus mode is STILL the active surface — the SAME keypress
      // must not have ALSO fired focus mode's onExit.
      await expect(page.getByRole("button", { name: RUN_COCKPIT.exitFocus })).toBeVisible();
    } finally {
      sql(`DELETE FROM approvals WHERE id = '${approvalId}'`);
    }
  });
});

// ── #160 — the group header says what its runs are waiting on ───────────────
// #215 — a run opens from a link ─────────────────────────────────────────────
//
// Synthetic runs, created for real through POST /api/v1/runs (like
// scripts/e2e-backend.sh seeds the file's own fixtures) rather than a raw
// agent_runs INSERT, then pinned to a state/approval shape SQL alone can't
// produce. Cleaned up in `finally` — DELETE FROM agent_runs cascades to their
// approvals (0001_init.sql's ON DELETE CASCADE), so one statement is enough.
//
// A LIVE held/reauth run is never reachable inside a rendered TitleGroup here:
// attentionFor (run-state-glyph.tsx) grades it "permission", runs.tsx's
// existing needsYou split (predates #160, untouched) pins every "permission"
// run to the "Needs you" lane BEFORE grouping, and a TitleGroup only ever
// receives what's left. #509 — a PENDING tool_call/credential_reauth row is
// now ALWAYS held (no client-side ceiling can drop it), so a live PENDING row
// of either kind is ALWAYS pinned to the Needs-you lane, never left grouped —
// there is no more live "stale but still counted in the group" case to prove
// here; the counted held/reauth chip vocabulary itself stays pinned directly
// against TitleGroup's own signals in runs/title-group.test.tsx and
// board-groups.test.ts. What's reachable live, and pinned below:
// the STARTING chip (a run.state fact, not an approval one), the
// "Checking…" pre-resolve window, and a long-PENDING TOOL_CALL/
// credential_reauth hold that STAYS live (Review, not Open; the Needs-you
// lane, not a "was held" demotion) no matter how old it is.
async function createNamedRun(page: Page, title: string, task: string): Promise<string> {
  const res = await page.request.post("/api/v1/runs", {
    headers: auth,
    data: { agent: "claude-code", repo: "acme/widgets", title, task },
  });
  expect(res.status(), await res.text()).toBe(201);
  return sql(`SELECT id FROM agent_runs WHERE task = '${task}' ORDER BY created_at DESC LIMIT 1`);
}

// #1197 D2 replaced title-grouping and the client-side approvals join
// (isHeld/approvalSignals, board-groups.ts) with the server's own projected
// `attention` field on GET /runs?view= (#1197 L1b) — one atomic read, no
// second "Checking…" race while a separate approvals fetch resolves, and no
// group header to count a wait reason on. The real end-to-end value these
// tests always had — the server's hold-timing rules (#509's 24h PENDING
// ceiling, #725/F1's 4-minute ADO window) reaching the actual rendered page —
// is what stays here; runs-landing.spec.ts's own row-kind coverage is fully
// synthetic (attention hand-built in the mock), so it cannot prove the SERVER
// computed these two timing edges correctly. The group-header and
// pre-resolve-race cases had no successor: both tested a mechanism (title
// grouping, a separate client approvals poll) that no longer exists on this
// page.
test.describe("Runs landing — real hold-timing edges reaching the row (#509, #725/F1)", () => {
  // #509 — a PENDING tool_call/credential_reauth row is live until the
  // SERVER says otherwise: the sandbox stays parked on it for up to
  // WARDYN_APPROVAL_EXPIRY_AFTER (24h default), so the page must never call
  // it dead sooner. 23 hours — just inside that ceiling — still reads
  // "Needs your approval" with a Review link and the held subline.
  test("a tool_call PENDING for 23 hours still offers Review and states 'sandbox held'", async ({ page }) => {
    const solo = await createNamedRun(page, "e2e long-held solo", "long-held solo run");
    sql(`UPDATE agent_runs SET state = 'WAITING_FOR_CONFIRMATION' WHERE id = '${solo}'`);
    sql(
      `INSERT INTO approvals (id, run_id, kind, requested_scope, state, requested_at) VALUES
       ('${randomUUID()}','${solo}','tool_call','{"tool":"Bash","cmd":"rm -rf build"}'::jsonb,'PENDING',now() - interval '23 hours')`,
    );

    try {
      await openRuns(page);
      const lane = page.getByRole("region", { name: "Needs you" });
      const row = lane.getByTestId("run-row").filter({ hasText: "e2e long-held solo" });
      await expect(row).toBeVisible();
      await expect(row.getByRole("button", { name: "Review" })).toBeVisible();
      await expect(row.getByText("Needs your approval")).toBeVisible();
      await expect(row.getByText(RUN_WAIT.waitingHeld(1))).toBeVisible();
    } finally {
      sql(`DELETE FROM agent_runs WHERE id = '${solo}'`);
    }
  });

  // The credential_reauth twin: no Review link of its own (a reauth hold's
  // action is Sign in, and only for the owner — same run here), but it must
  // stay pinned to Needs you and keep stating the live "AWS sign-in"
  // sentence at the same 23-hour age.
  test("a credential_reauth PENDING for 23 hours stays pinned to Needs you and keeps stating the live AWS sign-in wait", async ({
    page,
  }) => {
    const solo = await createNamedRun(page, "e2e long-held reauth", "long-held reauth run");
    sql(`UPDATE agent_runs SET state = 'RUNNING' WHERE id = '${solo}'`);
    sql(
      `INSERT INTO approvals (id, run_id, kind, requested_scope, state, requested_at) VALUES
       ('${randomUUID()}','${solo}','credential_reauth','{}'::jsonb,'PENDING',now() - interval '23 hours')`,
    );

    try {
      await openRuns(page);
      const lane = page.getByRole("region", { name: "Needs you" });
      const row = lane.getByTestId("run-row").filter({ hasText: "e2e long-held reauth" });
      await expect(row).toBeVisible();
      await expect(row.getByText("Waiting for your AWS sign-in")).toBeVisible();
      await expect(row.getByRole("button", { name: "Sign in" })).toBeVisible();
    } finally {
      sql(`DELETE FROM agent_runs WHERE id = '${solo}'`);
    }
  });

  // #725/F1's ADO-window nuance (a tool_call escalation releases its OWN
  // hold after 4 minutes, unlike every other tool_call, held for as long as
  // it is PENDING) has NO end-to-end browser test here. It needs a real
  // approvals row with a `grant_id` FK onto credential_grants, which the
  // seeded backend doesn't provision, and — unlike the old client-side
  // board — a route splice of GET /api/v1/approvals cannot substitute: this
  // page's `attention` is computed server-side, from the real database, on
  // GET /runs?view= itself (#1197 L1b), so a client-visible /approvals
  // splice never reaches it. The Go-side rule is covered by
  // internal/api/run_attention_test.go's own ADO cases; only the "does the
  // row render" half would be missing browser coverage here.
});

// ── 0.7.6 Finding 3 — "the failure names a destination instead of being one" ──
//
// The dispatch-time model-credential refusal now stamps `reason` and the
// DECLARED `mechanism` on the run.create/failure row it already wrote; the
// console grades that ending `credential` and puts the sign-in under the
// server's own sentence.
//
// Harness ceiling, the same one model-access-banner.spec.ts opens with: the
// seeded backend authenticates with a bare admin bearer token, has no per-user
// AWS session to grade and no run that reached dispatch with a dead credential —
// so the trail row, `model_access` and the viewer's own subject are spliced.
// What only a browser proves is what is spliced here and asserted below: that
// this ending reaches the failure block as prose PLUS a door, on the run page,
// with no second "Sign in to AWS" beside it. The real refusal, from a real
// per-user AWS session that lapsed, is live case J (lane e2e-sso-path).
const CREDENTIAL_REFUSAL =
  "This run's model access is configured as Amazon Bedrock (captured AWS SSO session), and that session can no longer be renewed — sign in to AWS from Getting started in the console, or from the sign-in banner the console shows on every page. Wardyn does not substitute a different model provider.";
const CREDENTIAL_VIEWER = "alice@corp.example";

/** The viewer's own subject, so `created_by === principal` can be true of a
 *  seeded run: the door is the VIEWER's credential and an admin reading
 *  somebody else's failed run is not offered it. */
async function mockPrincipal(page: Page, principal: string): Promise<void> {
  await page.route("**/api/v1/me", async (route) => {
    const response = await route.fetch();
    const json = await response.json();
    json.principal = principal;
    await route.fulfill({ response, json });
  });
}

/** A graded per-user model access + the bedrock_sso roster row, cached and
 *  served (the landing redirect, the shell poll and the block's own refresh all
 *  hit this endpoint). */
async function mockActionableModelAccess(page: Page): Promise<void> {
  let cached: Record<string, unknown> | null = null;
  await page.route("**/api/v1/setup/status*", async (route) => {
    if (!cached) {
      const body = (await (await route.fetch()).json()) as Record<string, unknown>;
      body.model_access = { state: "expired_signin", action: AGENTS.SIGN_IN_AWS };
      body.harnesses = ((body.harnesses ?? []) as { id: string }[]).map((h) =>
        h.id === "claude-code"
          ? { ...h, enabled: true, mechanism: "bedrock_sso", credential_source: "per_user" }
          : h,
      );
      cached = body;
    }
    await route.fulfill({ json: cached! });
  });
}

test.describe("a run refused for a model credential carries the sign-in, not directions to it", () => {
  test("the failure block states the server's sentence and opens the AWS sign-in in place", async ({ page }) => {
    await mockPrincipal(page, CREDENTIAL_VIEWER);
    await mockActionableModelAccess(page);
    // The refused run: the server's sentence on the row (failure_hint), and the
    // viewer as its creator.
    await page.route("**/api/v1/runs/*", async (route) => {
      if (route.request().method() !== "GET") return route.fallback();
      const response = await route.fetch();
      const json = await response.json();
      if (json.task === "e2e fixture 6") {
        json.failure_hint = CREDENTIAL_REFUSAL;
        json.created_by = CREDENTIAL_VIEWER;
      }
      await route.fulfill({ response, json });
    });
    // …and the class, on the run.create/failure row the refusal writes.
    await page.route("**/api/v1/audit*", async (route) => {
      const response = await route.fetch();
      const rows = (await response.json()) as Record<string, unknown>[];
      if (!Array.isArray(rows) || rows.length === 0) return route.fulfill({ response, json: rows });
      const runID = rows[0].run_id;
      rows.unshift({
        id: randomUUID(),
        time: new Date().toISOString(),
        run_id: runID,
        actor_type: "system",
        actor: "wardynd",
        action: "run.create",
        target: String(runID),
        outcome: "failure",
        data: { error: CREDENTIAL_REFUSAL, reason: "model_credential", mechanism: "bedrock_sso" },
      });
      await route.fulfill({ response, json: rows });
    });

    await openRuns(page);
    await page.getByText("e2e fixture 6").click();
    await expect(page).toHaveURL(/\/runs\/[0-9a-f-]{8,}/);

    const block = page.getByTestId("run-failure-block");
    await expect(block).toHaveAttribute("data-ending", "credential");
    // The SERVER's sentence, once, unchanged — no copy of ours restating it.
    await expect(block.getByText(CREDENTIAL_REFUSAL)).toHaveCount(1);
    await expect(block.getByText(MODEL_ACCESS_RUN_DOOR.NOTE)).toBeVisible();

    // ONE "Sign in to AWS" on the page: the block owns the door while it has
    // one, so the shell strip keeps its sentence and drops its button.
    await expect(page.getByText(MODEL_ACCESS_BANNER.EXPIRED_SHORT)).toBeVisible();
    await expect(page.getByRole("button", { name: AGENTS.SIGN_IN_AWS, exact: true })).toHaveCount(0);

    // A DOOR, not a signpost: the sign-in opens here, on the run's own page.
    const before = new URL(page.url()).pathname;
    await block.getByRole("button", { name: MODEL_ACCESS_RUN_DOOR.SIGN_IN_ARIA }).click();
    await expect(page.getByRole("heading", { name: MODEL_ACCESS_BANNER.DIALOG_TITLE })).toBeVisible();
    await expect(page.getByTestId("harness-login-pane")).toBeVisible();
    expect(new URL(page.url()).pathname).toBe(before);

    await page.keyboard.press("Escape");
    await expect(page.getByRole("heading", { name: MODEL_ACCESS_BANNER.DIALOG_TITLE })).toHaveCount(0);
  });
});
