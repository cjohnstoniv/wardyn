/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, gotoConsole, navTo } from "./fixtures";
import { RUN } from "../src/app/components/wardyn/copy";
import { LOGIN_SANDBOX_NOTE } from "../src/app/components/screens/run-detail/login-sandbox-note";
import { STATES } from "../src/app/components/wardyn/states";
import type { Page } from "@playwright/test";

// Split out of runs.spec.ts (#209): the addressable /runs/:id detail hub's
// own identity/terminal/kill/clone rendering, the login-sandbox note, the
// approvals-scoped-on-the-wire check and the side-fetch-failure resilience
// case, against the seeded 9-fixture backend runs.spec.ts's own top comment
// maps out (fixture N -> state). None of this file's suites mutate run
// state — the kill/hold-timing suites stayed in runs.spec.ts.
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
