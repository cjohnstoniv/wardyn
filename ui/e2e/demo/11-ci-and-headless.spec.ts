/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * V11 · CI & headless — the BROWSER half (beats 5-6).
 *
 * This video is a hybrid, and this file is only its back end. Beats 1-4 are a
 * host shell — the CI policy file, one `scripts/ci-run.sh` invocation, its exit
 * code, its artifacts — and live in scripts/demo-beats/11-ci-and-headless.sh,
 * filmed under the same screen grab by `record-demo.sh --terminal-script`. The
 * two segments are joined, terminal first, into one mp4. Read that script
 * first: everything this file inherits was set up there.
 *
 * WHAT IT FILMS. The pipeline's run, on the console board, opened, and its
 * audit trail — the closing argument of the whole series: an unattended run
 * leaves exactly the same receipts an attended one does.
 *
 * THE STACK IS NOT :8080. Beats 1-4 brought up a SEPARATE compose stack
 * (WARDYN_CI_PROJECT=wardyn-ci-demo, WARDYN_UP_PORT=8099) and left it running
 * with WARDYN_CI_KEEP=1. That stack is what this films, so every navigation
 * here is an ABSOLUTE url rather than a baseURL-relative one — playwright.
 * config's demo project points at :8080, which is the series stack and has an
 * entirely different set of runs on it.
 *
 * AUTH IS A PRE-SEEDED ADMIN TOKEN, NOT LOCAL MODE. The rest of the series
 * films a local-mode stack with no sign-in at all (SV1). The CI overlay brings
 * up postgres + wardynd with an admin bearer and no dex, so there is nothing to
 * sign in TO — and adding WARDYN_LOCAL_MODE to the CI stack would mean the
 * video films a configuration no real pipeline runs (DA10, superseded by Track
 * D's round-4 ruling). Instead the token lands in sessionStorage before first
 * navigation: lib/api/core.ts's getToken() reads `ssGet(TOKEN_KEY) ??
 * lsGet(TOKEN_KEY)`, sessionStorage first, so the app mounts already
 * authenticated and no login wall is ever filmed.
 *
 * IT READS run.json RATHER THAN GUESSING. The terminal half wrote
 * ci-artifacts/run.json; this half reads the run id, state and task straight
 * out of it. So the two lanes agree on which run they are talking about
 * because they read the file the pipeline actually produced — not because the
 * same uuid got typed into two places. It is also the check that catches the
 * worst failure this video has available to it: filming the WRONG STACK, where
 * every locator would still match something and the take would look fine.
 *
 * This is NOT a test. It asserts only enough to keep itself honest and to know
 * when to advance; a failure here means the recording is wrong, not that the
 * product is broken.
 *
 * Selectors are getByRole + accessible names, matching the rest of the suite.
 */

import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { test, expect, type Page } from "@playwright/test";
import { act, beat, caption, PACE, spotlight } from "./overlay";
// The browser, the recorded context and the shared page live in stage.ts:
// importing it is what registers this file's beforeAll/afterAll, and each beat
// reads the page out of stage() rather than closing over a module-level `let`.
import { stage } from "./stage";

test.skip(!process.env.WARDYN_DEMO, "demo recording — run via `make record-demo` (exports WARDYN_DEMO=1)");

test.describe.configure({ mode: "serial" });

// ---------------------------------------------------------------------------
// The CI stack, and the receipts beats 1-4 left behind
// ---------------------------------------------------------------------------

/**
 * The console the pipeline stood up. Deliberately local to this file: no other
 * video in the series talks to a second stack, so this does not belong in
 * task.ts. Mirrors the beat script's WARDYN_UP_PORT default.
 */
// 127.0.0.1, not localhost: docker-compose.yaml publishes the console as
// `127.0.0.1:${WARDYN_UP_PORT}:8080` — an IPv4 loopback bind with nothing
// listening on ::1 — and `localhost` resolves to ::1 first on this box. The
// beat script's own reachability probe uses the same literal.
const CI_STACK = process.env.WARDYN_CI_URL || `http://127.0.0.1:${process.env.WARDYN_UP_PORT || "8099"}`;

/** The bearer the CI overlay's wardynd was started with (scripts/ci-run.sh). */
const CI_ADMIN_TOKEN = process.env.WARDYN_ADMIN_TOKEN || "demo-admin-token";

/** lib/api/core.ts's TOKEN_KEY. Same string in both storages; ss wins. */
const TOKEN_KEY = "wardyn_admin_token";

/** The owner's staccato lines read fast; PACE.read after one is dead air. */
const BEAT_SHORT = 1400;

// A real console boot against a stack that has just finished a run: the app
// shell, the auth probe, and the board's first poll. A ceiling for waiting on
// the PRODUCT — the pacing the viewer sees comes from overlay.ts.
const BOARD_SETTLES = 60_000;

const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../../..");
const RUN_JSON = path.resolve(REPO_ROOT, process.env.WARDYN_CI_OUT || "ci-artifacts", "run.json");

/**
 * The run the pipeline actually launched, straight off disk.
 *
 * Loud on absence, because the alternative is a driver that shrugs and films
 * the board of whatever stack happens to answer. If this file is missing, beats
 * 1-4 either did not run or did not get far enough to collect artifacts, and
 * there is nothing for beats 5-6 to be about.
 */
function pipelineRun(): { id: string; state: string; task: string } {
  if (!fs.existsSync(RUN_JSON)) {
    throw new Error(
      `no ${RUN_JSON} — beats 5-6 film the run beats 1-4 launched, and that is where its id comes from. ` +
        `Record this video with: scripts/record-demo.sh --video 11 --terminal-script scripts/demo-beats/11-ci-and-headless.sh`,
    );
  }
  const run = JSON.parse(fs.readFileSync(RUN_JSON, "utf8")) as { id?: string; state?: string; task?: string };
  if (!run.id || !run.task) throw new Error(`${RUN_JSON} carries no run id/task — the pipeline never finished collecting.`);
  // Beat 5 narrates a COMPLETED badge and beat 6 an `exit 0` chip. On a
  // FAILED/KILLED run both are false, and asserting them on screen would burn
  // 60s of BOARD_SETTLES waiting for a badge that will never render. record-
  // demo.sh does NOT abort the browser lane when the beat script exits
  // non-zero (it only marks the take incomplete at the end), so this is the
  // only thing standing between a bad pipeline run and two filmed minutes of it.
  if (run.state !== "COMPLETED") {
    throw new Error(
      `${RUN_JSON} says state=${run.state || "(none)"} — beats 5-6 narrate "Completed" and "exit zero". ` +
        `The pipeline run did not succeed; fix it and re-shoot rather than filming the board over a failure.`,
    );
  }
  return { id: run.id, state: run.state, task: run.task };
}

// ---------------------------------------------------------------------------
// Beat 5 — on the board
// ---------------------------------------------------------------------------

test("beat 5 — the pipeline's run, on the board", async () => {
  test.setTimeout(300_000);
  const page = stage();
  const run = pipelineRun();

  // Seeded BEFORE the first navigation of the whole take, which is what makes
  // it work: addInitScript runs on every document this page loads, so the token
  // is in sessionStorage before the app's mount-time auth probe fires and the
  // sign-in screen never renders. (stage.ts's own WARDYN_DEMO_TOKEN seeding
  // writes localStorage for the :8080 stack; getToken prefers sessionStorage,
  // so this wins even when both are set.)
  await page.addInitScript(
    ([key, tok]) => {
      try {
        sessionStorage.setItem(key, tok);
      } catch {
        /* private mode — ignore */
      }
    },
    [TOKEN_KEY, CI_ADMIN_TOKEN],
  );

  await page.goto(`${CI_STACK}/runs`);
  await page.bringToFront();

  // Fail here rather than two minutes into a silent, caption-less take: every
  // narration call degrades to a no-op by design, so nothing downstream would
  // ever complain about an overlay that failed to install. (Same guard the
  // walkthrough's first act carries, for the same reason.)
  await expect
    .poll(() => page.evaluate(() => typeof (window as unknown as Record<string, unknown>).__demo), { timeout: 15_000 })
    .toBe("object");

  // A CLI run carries no title, so the board's card headline is its TASK
  // (runHeadline: `title || task`), and an untitled run never forms a title
  // group — it lands in the trailing ungrouped grid rather than under a "Done"
  // section header. What the beat films is the single card and its state badge.
  const headline = page.getByText(run.task, { exact: true });
  await expect(headline).toBeVisible({ timeout: BOARD_SETTLES });

  // THE WRONG-STACK GUARD. The series stack on :8080 is full of runs from the
  // other nine videos; this one has exactly the run the pipeline just made. If
  // a stale port-forward or an inherited WARDYN_DEMO_BASE_URL pointed us at the
  // wrong console, every locator below would still match something and the take
  // would look perfect.
  await expect(headline).toHaveCount(1);

  await caption(page, "Now let's look at that same run in the console.");
  await spotlight(page, headline);
  // The payoff of the whole terminal half, asserted where it is claimed: the
  // badge renders runStateMeta's TITLE-CASE label ("Completed"), not the wire
  // enum the artifacts carry.
  await expect(page.getByText("Completed", { exact: true }).first()).toBeVisible({ timeout: BOARD_SETTLES });
  await beat(page, PACE.read);
  await caption(page, "It looks like any other run.");
  await beat(page, BEAT_SHORT);
  await caption(page, "That's the point.");
  await beat(page, BEAT_SHORT);
  await caption(page, "CI doesn't get a special security model.");
  await beat(page, PACE.read);
  await caption(page, "It gets the same one.");
  await beat(page, BEAT_SHORT + 400);
  await spotlight(page, null);
});

// ---------------------------------------------------------------------------
// Beat 6 — same trail, no human
// ---------------------------------------------------------------------------

test("beat 6 — same trail, no human", async () => {
  test.setTimeout(360_000);
  const page = stage();
  const run = pipelineRun();

  // The card is a plain div with an onClick (RunCard dropped role="button"
  // rather than nest real buttons inside a widget role), so the headline
  // paragraph inside it is the honest click target — the click bubbles.
  await act(page, page.getByText(run.task, { exact: true }));
  // Not just "a run page": THIS run's page, on THIS stack, by the id the
  // pipeline itself wrote into run.json.
  await expect(page).toHaveURL(`${CI_STACK}/runs/${run.id}`, { timeout: 30_000 });

  // The command bar's exit-code chip is audit-derived (exitCodeFromAudit reads
  // run.complete's data.exit_code), which is exactly why it is worth filming:
  // the number the pipeline exited with and the number on this chip come from
  // the same event, not from two systems that agree by luck.
  const exitChip = page.getByText("exit 0", { exact: true });
  await expect(exitChip).toBeVisible({ timeout: 60_000 });
  await caption(page, "The run finished with exit zero.");
  await spotlight(page, exitChip);
  await beat(page, BEAT_SHORT + 400);
  await spotlight(page, null);

  // SAY-ON-CLICK: "Open the audit." — spoken on the click that opens the tab.
  await act(page, page.getByRole("tab", { name: /Audit/ }), "Open the audit.");

  // Assert the trail the narration is about to describe. `run.create` through
  // `run.complete` is the claim; both are real actions on the success path
  // (internal/api/runs.go, internal/api/runs_lifecycle.go) and the tab prints
  // them raw and dotted.
  await expect(page.getByText("run.create", { exact: true }).first()).toBeVisible({ timeout: 60_000 });
  await expect(page.getByText("run.complete", { exact: true }).first()).toBeVisible({ timeout: 60_000 });
  // And "append-only" is the tab's own words, not ours — spotlight the header
  // that says it while the line is spoken. Substring-matched, not anchored: the
  // row that carries the count also carries the "open full Audit" link, so its
  // textContent is never just the sentence.
  const appendOnly = page.getByText(/Append-only · \d+ events? for this run/).first();
  await expect(appendOnly).toBeVisible({ timeout: 30_000 });
  await spotlight(page, appendOnly);
  await caption(page, "There's the append-only trail.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Created.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Executed.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Completed.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Nobody watched this one live.");
  await beat(page, BEAT_SHORT);
  await caption(page, "But the record is still there.");
  await beat(page, BEAT_SHORT + 400);
  await spotlight(page, null);

  // ── conclusion ───────────────────────────────────────────────────────────
  // The series motif ("Sandboxed. Governed. Self-hosted. Free.") opens on V02's
  // hero and closes the series on V12; it is deliberately absent here — V03
  // through V11 never speak it.
  await caption(page, "A pipeline doesn't need a human to enforce a policy.");
  await beat(page, PACE.read);
  await caption(page, "The policy makes the decision.");
  await beat(page, BEAT_SHORT);
  // P9 (dialog review, owner-ratified 2026-08-23): "verdict" is now episode
  // 09's word for a replay chip ("Replayed clean" / "Replayed — caught N").
  // Two episodes apart, one word, two meanings — so it leaves BOTH lanes here
  // (the terminal half's "That result becomes the build's verdict." went with
  // it: scripts/demo-beats/11-ci-and-headless.sh).
  await caption(page, "The run either fits inside the policy or it doesn't, and it says which.");
  await beat(page, PACE.read);
  await caption(page, "The pipeline carries that result forward.");
  await beat(page, BEAT_SHORT);
  // DIALOG-NEW-BEAT (dialog review, A17): the conclusion states what a
  // pipeline GETS but never what happens when it reaches off-list — the beat
  // the terminal half films (scripts/demo-beats/11-ci-and-headless.sh, "No
  // reviewer. / No approval screen. / The build goes red."), restated here
  // where the episode sums itself up. Drafted; see
  // local/light-episodes-dialog-flags.md.
  await caption(
    page,
    "And if it reaches for something off the list, there's no approval step to wait for. The policy fails closed, and the pipeline gets that failure as its result.",
  );
  await beat(page, PACE.read);
  await caption(page, "Governance that doesn't require somebody to stay awake.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Next, we're going to look at the record itself.");
  await beat(page, PACE.read);
  await caption(page, "And ask the final question:");
  await beat(page, BEAT_SHORT);
  await caption(page, "Can we actually prove what happened?");
  await beat(page, PACE.read + 400);
  await caption(page, "");
  await silentCard(page, "Next — 12: Audit & attach");
});

/** The unspoken outro card, per the series convention video 01 set. */
async function silentCard(page: Page, text: string): Promise<void> {
  const set = (t: string) =>
    page
      .evaluate((s: string) => {
        const d = (window as unknown as Record<string, Record<string, (...x: unknown[]) => void>>).__demo;
        d?.chapter?.(s, "");
      }, t)
      .catch(() => {
        /* overlay absent — a card is cosmetic, never fatal */
      });
  await set(text);
  await page.waitForTimeout(PACE.chapter);
  await set("");
}
