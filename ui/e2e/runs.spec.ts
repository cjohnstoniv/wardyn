/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { randomUUID } from "node:crypto";
import { test, expect, ADMIN_TOKEN, gotoConsole, navTo, sidebarLink, sql } from "./fixtures";
import { RUN_WAIT } from "../src/app/components/wardyn/copy/run-wait";
import { NO_BARRIER } from "../src/app/components/wardyn/copy";
import type { Page } from "@playwright/test";

// ============================================================================
// Runs lane: the Runs landing page (#1197 D2, rewritten from the old board +
// table density switch) + kill, against the seeded 9-fixture backend below.
// #209 split this file by behaviour: the addressable /runs/:id detail hub
// lives in runs-detail.spec.ts, the header's own status chips in
// runs-header.spec.ts, and the cockpit widgets (layout catalog, failure
// block sizing, attach card, focus mode, the model-credential refusal door)
// in runs-cockpit.spec.ts. All four files share this same fixture map.
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
// run detail hub (runs-detail.spec.ts), Kill and Clone having moved onto the
// run page.
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

// #1197 D2 (design.md §4): the kebab menu (Kill, Clone, Open) is gone from
// the Runs landing page — Kill and Clone moved onto the run page, one inline
// action per row at most. Their coverage is now entirely on that page (the
// "Killing an active run" tests below, and the clone-door tests in
// runs-detail.spec.ts), so this describe block has no replacement here.

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
      headers: { Authorization: `Bearer ${ADMIN_TOKEN}` },
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

test.describe("#214 — no barrier: the shell banner and the top bar's route", () => {
  test("the shell banner names the blocker and routes to the Environment step; New run stays reachable", async ({
    page,
  }) => {
    // The Admin Setup funnel shows the welcome hero first until this is set
    // (confinement-posture.spec.ts's own precedent) — this test clicks
    // through into the funnel itself, not just to its URL.
    await page.addInitScript(() => {
      try {
        localStorage.setItem("wardyn-onboarding-seen", "1");
      } catch {
        /* private mode — ignore */
      }
    });
    // Cache-and-serve, not route.fetch()+refulfill per match (new-run.spec's
    // spliceNoBarrier, #1365): the Environment step this test clicks through
    // to re-reads /setup/status, and a round trip per match raced Playwright
    // disposing an in-flight route's response ("Response has been disposed").
    let cached: Record<string, unknown> | null = null;
    await page.route("**/api/v1/setup/status*", async (route) => {
      if (!cached) {
        const json = await (await route.fetch()).json();
        json.runner = { ...json.runner, driver: "docker", confinement_classes: [] };
        cached = json;
      }
      await route.fulfill({ json: cached! });
    });
    await gotoConsole(page);

    await expect(page.getByText(NO_BARRIER.BANNER_TITLE)).toBeVisible();
    const routes = page.getByRole("link", { name: NO_BARRIER.CTA });
    await expect(routes).toHaveCount(2); // the shell banner + the top bar
    for (const link of await routes.all()) {
      await expect(link).toHaveAttribute("href", NO_BARRIER.ADMIN_ROUTE);
    }

    // New run stays live — disabling it would hide the explanation behind
    // the control that carries it.
    await expect(page.getByRole("button", { name: "New run" })).toBeEnabled();

    // #1328 review round 2, R2-1 — the seeded backend is a single-operator
    // install ("url" access): console-view.tsx's viewVerdict `pass`es
    // /admin/setup straight through for it. This proves the real
    // destination, not just the URL — the Environment step itself.
    await routes.first().click();
    await expect(page).toHaveURL(/\/admin\/setup\?step=environment/);
    await expect(page.getByRole("heading", { name: "Pick your barrier", level: 2 })).toBeVisible();
  });

  // The seeded backend's own default (`-runner none`) already reads
  // no-barrier for the shell (deriveReadiness counts confinement_classes,
  // empty either way), so the negative case needs its own splice too — a
  // real driver reporting at least one class.
  test("stays silent when a barrier is available", async ({ page }) => {
    await page.route("**/api/v1/setup/status*", async (route) => {
      try {
        const response = await route.fetch();
        const json = await response.json();
        json.runner = { ...json.runner, driver: "docker", confinement_classes: ["CC1"] };
        await route.fulfill({ response, json });
      } catch {
        // The test ended while the real answer was in flight.
      }
    });
    await gotoConsole(page);
    await expect(page.getByText(NO_BARRIER.BANNER_TITLE)).toHaveCount(0);
    await expect(page.getByRole("link", { name: NO_BARRIER.CTA })).toHaveCount(0);
  });
});
