/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * Waiting on a run that can END UNDER YOU, and booting one on camera.
 *
 * WHY THIS IS ITS OWN MODULE AND NOT demos.ts. Every helper below was already
 * written twice — waitUnlessGone lives in 10-approvals-and-egress.spec.ts:202
 * and 09-record-a-run.spec.ts inlines the same race FIVE times — and episodes
 * 00 and 04d need all of it again. demos.ts would have been the obvious home,
 * except that it sits in the import closure of 02c and all four 03x specs:
 * under H6's re-record impact rule (scripts/demo-rerecord-impact.py, leg (a):
 * "anything in the spec's ui/e2e/demo/ import closure"), editing demos.ts puts
 * five already-graded takes back in front of the camera for a helper none of
 * them calls. A sibling module is what both scripts proposed for exactly that
 * reason ("demos.ts — or a sibling"), and it costs nothing: 09 and 10 keep
 * their own copies until they are re-shot for a reason of their own, and this
 * file is where the next episode reaches instead of writing a seventh copy.
 *
 * THE ONE IDEA HERE. Every approval surface, terminal and audit panel in the
 * console renders only while its run is RUNNING (run-detail.tsx's terminalPane
 * is gated on `run.state === "RUNNING"`; record-pane.tsx's SessionCard mounts
 * the live half only in its `recording`/`replaying` stages). So a sandbox that
 * dies — a failed start, an auto-stop, a stray kill — takes the row, the
 * terminal and the panel with it, and a bare wait then polls for minutes
 * against a node that can no longer appear. V05 lost a take to exactly that.
 * Racing the two mutually-exclusive outcomes turns it into an immediate,
 * NAMED failure instead.
 */

import { expect, type Locator, type Page } from "@playwright/test";
import { beat, ffwdEnd, ffwdStart, PACE } from "./overlay";

/** Real containers: a ceiling for waiting on the PRODUCT, never pacing. */
export const RUN_BOOTS = 240_000;

/** RunStateBadge's terminal labels — TITLE CASE, from runStateMeta
 *  (ui/src/app/components/wardyn/primitives.tsx). */
export const RUN_OVER = /^(Completed|Failed|Stopped|Killed)$/;

/**
 * Every chip a confined replay can END on (STAGE_CHIP_META + replayedChipMeta,
 * record-pane-chips.tsx). All four mean "the replay is over"; missing one puts
 * a wait back to polling for a node that can no longer exist. The em-dash is
 * the same U+2014 the component emits.
 */
export const REPLAY_OVER = /^(Replayed clean|Replayed — caught \d+|Replayed confined|Replay failed)$/;

/**
 * Await `want`, unless `gone` lands first — then fail loudly, saying which.
 *
 * `want` is any Playwright wait already in flight (an `expect(…)` assertion or
 * a `locator.waitFor`), so this covers toBeVisible, toContainText and a raw
 * waitFor without a second helper. The losing promise keeps polling in the
 * background until its own timeout; both branches carry a rejection handler, so
 * that is a dangling poll and never an unhandled rejection.
 */
export async function waitUnlessGone(
  want: Promise<unknown>,
  gone: Locator,
  timeout: number,
  why: string,
): Promise<void> {
  const outcome = await Promise.race([
    want.then(
      () => "ok" as const,
      () => "failed" as const,
    ),
    gone.waitFor({ state: "visible", timeout }).then(
      () => "gone" as const,
      () => "timeout" as const,
    ),
  ]);
  expect(
    outcome,
    outcome === "gone"
      ? `${why} — the run ENDED before this beat could land.`
      : `${why} — the wait timed out (${timeout / 1000}s) with the run still live.`,
  ).toBe("ok");
}

/**
 * The silent stretch between "Launch run" and a terminal you can type into.
 *
 * POST /runs dispatches SYNCHRONOUSLY (runs_dispatch.go: "dispatch is invoked
 * synchronously from the create-run handler"), so the navigate to /runs/<id>
 * does not happen until the container is provisioned: the URL change and the
 * terminal mount are ONE stretch of dead air, which is why the URL wait carries
 * RUN_BOOTS and not a minute.
 *
 * NOTHING IS SPOKEN INSIDE THE SPAN. A line narrated inside a fast-forward is a
 * line played over frames the encoder is about to throw away — the voice would
 * land 12x early and every cue after it would be wrong (overlay.ts's ffwd
 * contract). The caller's last caption is given 200ms to finish first, for the
 * same reason.
 *
 * Returns the run's URL, so an episode that leaves the cockpit can come back to
 * the same run later (episode 00 does, twice).
 */
export async function bootRun(page: Page, why: string): Promise<string> {
  await beat(page, 200);
  await ffwdStart(page);
  try {
    await expect(page, `${why} — /runs/new never navigated to a run`).toHaveURL(/\/runs\/[0-9a-f-]{8,}/i, {
      timeout: RUN_BOOTS,
    });
    await waitUnlessGone(
      expect(page.locator(".xterm-screen").first()).toBeVisible({ timeout: RUN_BOOTS }),
      page.getByText(RUN_OVER).first(),
      RUN_BOOTS,
      why,
    );
  } finally {
    // Real time resumes the instant the terminal is on screen — even on a
    // failed take, so the encoder gets a span this run actually spent.
    await ffwdEnd(page);
  }
  // .xterm-screen renders when AttachTerminal MOUNTS, before the PTY websocket
  // is up (attach-terminal.tsx only sends on readyState OPEN); typing here eats
  // the first characters and the shell reports "command not found" on camera.
  await beat(page, PACE.read);
  return page.url();
}
