/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * Video 05 of the series — "What it stops".
 *
 * INTERIM FILENAME. This file is "05a-what-it-stops.spec.ts", not
 * "05-what-it-stops.spec.ts": the current 05-autonomous-agent.spec.ts still
 * holds that number until the 12-episode series renumber (a separate,
 * mechanical commit) lands. record-demo.sh's --video flag validates
 * `^[0-9]{2}$`, so "05a" cannot be passed to it yet — this file is not
 * launchable until that renumber frees the real "05". That is by design, not
 * an oversight.
 *
 * WHAT THIS FILMS. The four keyless guardrail demos that used to close out
 * "Getting started" (01-getting-started.spec.ts) — the sealed box, fail-then-
 * approve, held-at-the-door, and lines-that-can't-be-crossed, including the
 * 169.254.169.254 metadata probe inside the last one — now as their own
 * episode. This is the payoff video: 01 proved a barrier, a network path and
 * a secret store; this one proves what having them actually buys you. It is a
 * refusals episode — every beat is the boundary hurting something on purpose,
 * on camera, under a policy the viewer can read. The fifth original demo,
 * once-or-for-good, is CUT here: its caret-scope beat belongs to the
 * approval-scopes episode (old 07), which owns that whole subject.
 *
 * THE DIALOG IS THE OWNER'S, VERBATIM (rewrite of 2026-08-21, from
 * local/episodes-03-12-scripts-current.md's "Episode 05 — What it stops"
 * section, as edited). Every SAY / SAY-ON-CLICK / SAY-ON-DECIDE stanza in
 * that section is one caption() (or one act()/decide() spoken-text argument)
 * here, in order — do not reword; wording changes go through the script file
 * and the owner. Short stanzas ride BEAT_SHORT; full-length lines keep
 * PACE.read. The owner's four tests (Refuse, Ask, Hold, and an unnamed
 * fourth) map onto STOP_DEMOS as sealed-box, fail-then-approve, and
 * held-at-the-door, in order — except the fourth test's own dialogue is not
 * one iteration: its wikipedia-then-Deny half rides the TAIL of the
 * held-at-the-door iteration (that's where the product's own demo card
 * already carries that step — demo-catalog.ts's "held-at-the-door" entry),
 * and its cloud-metadata half is the body of the lines-that-cant-be-crossed
 * iteration. sealed-box's SAY-ON-DECIDE ("The request is denied.") has no
 * real decide() to attach to — that demo's policy is always_deny with no
 * human in the loop — so it is spoken as a plain caption at the moment the
 * refusal actually lands on screen.
 *
 * SPLIT PROVENANCE (2026-08-20, owner-approved). This is an extraction of
 * 01-getting-started.spec.ts's pre-split act 3 (itself moved from
 * walkthrough.spec.ts act 3 before that), not a rewrite: the per-demo
 * choreography, the arithmetic waiting out each curl, and the post-approval
 * payoff assertions are all proven on camera and moved verbatim. What's new
 * is act 1 (getting back to this point in a fresh browser session — see
 * STAGING below), the once-or-for-good cut, and the persona-adjudicated edits
 * from local/demo-review-2026-08-20/ADJUDICATION.md's "V01" section that land
 * on beats now filmed here.
 *
 * This is NOT a test. It asserts only enough to keep itself honest and to know
 * when to advance; a failure here means the recording is wrong, not that the
 * product is broken. It runs against the REAL compose stack on :8080 with real
 * sandboxes — the hermetic `-runner none` e2e backend cannot start one at all.
 *
 * STAGING THE OPERATOR OWNS (off camera, before the take rolls):
 *   1. NO RESET. Unlike 01, this take runs `record-demo.sh --no-reset` (SV20)
 *      against the SAME long-lived stack the earlier videos left behind — the
 *      barrier pick and the Secrets store carry over from this host's own
 *      state. Series ruling S6 (stale-stack hygiene) still governs whatever a
 *      later video in the real running order leaves for this one; these four
 *      demos leave nothing pending themselves — both approve-demos in this
 *      file are decided, not left hanging (the beat that used to leave one
 *      open was once-or-for-good's re-raise, cut above).
 *   2. A FRESH BROWSER CONTEXT. stage.ts registers its own beforeAll/afterAll
 *      per spec FILE, so this take does NOT continue mid-DOM from wherever 01
 *      left the funnel — it reopens /setup and walks back in. Act 1 below
 *      owns that reentry, including the one piece of state that is
 *      deliberately NOT durable across a fresh session: Network's
 *      connectivity proof (steps.ts's CorpNetworkState is session-only by
 *      design — "never a stale reached surviving a page reload").
 *
 * Selectors are getByRole + accessible names, matching ui/e2e/fixtures.ts and
 * the rest of the suite: a copy change breaks this loudly and in one place,
 * markup churn does not break it at all.
 */

import { test, expect, type Page } from "@playwright/test";
import { FUNNEL_DEMOS } from "./task";
import { act, beat, caption, centerInFrame, chapter, PACE, spotlight, typeInTerminal } from "./overlay";
// The browser, the recorded context and the shared page live in stage.ts:
// importing it is what registers this file's beforeAll/afterAll, and each act
// reads the page out of stage() rather than closing over a module-level `let`.
import { stage } from "./stage";
import { advance, decide } from "./funnel";

test.skip(!process.env.WARDYN_DEMO, "demo recording — run via `make record-demo` (exports WARDYN_DEMO=1)");

// Sandboxes are real containers — the connectivity re-proof and every one of
// the four demos launches one. These are minutes, not seconds. They are
// ceilings for waiting on the PRODUCT; the pacing the viewer sees comes from
// overlay.ts. (funnel.ts's decide() has its own APPROVAL_APPEARS ceiling for
// a held request, since that one waits on a HUMAN.)
const SANDBOX_UP = 180_000;

/** Hold for a SHORT stanza — the owner's staccato lines drag on PACE.read. */
const BEAT_SHORT = 1400;

test.describe.configure({ mode: "serial" });

// ---------------------------------------------------------------------------
// The four guardrail demos, as this episode films them.
//
// FUNNEL_DEMOS (task.ts) is the shared definition — same ids, same commands,
// same captions the walkthrough proved. once-or-for-good is filtered out: its
// caret-scope beat belongs to the approval-scopes episode (old 07), which
// owns that whole subject.
//
// The spoken dialogue for each of the owner's four tests is inlined directly
// in the V05a act 2 loop below rather than kept in a per-demo table: the
// owner's rewrite doesn't split evenly into "intro" / "property" / "policy
// line" buckets per demo (Test 4's own dialogue, for instance, spans the tail
// of the held-at-the-door iteration and the body of the lines-that-cant-be-
// crossed one — see the file header). The policy block is still SHOWN (a
// silent spotlight) but no longer narrated on its own; the owner's plain-
// language lines already say what changed.
//
// `lines-that-cant-be-crossed` loses its third command and shortens the
// second (both pre-existing local trims, unrelated to the dialogue rewrite):
// the 192.168.1.1 probe teaches nothing the metadata probe did not already
// teach, and --max-time 5 twice is dead air this episode does not need.
// ---------------------------------------------------------------------------

type StopDemo = {
  id: string;
  label: string;
  cmds: readonly string[];
  caption: string;
  approve: boolean;
  scope: "run" | "once";
};

/** Same two beats as the card's first two steps, minus the LAN probe. The
 *  first (example.com, proving the policy is wide open) plays silently — no
 *  owner line covers it; it's the visual contrast for the metadata refusal
 *  that follows. */
const LINES_CMDS = [
  "curl -sSI https://example.com",
  "curl -sSI --max-time 2 http://169.254.169.254/latest/meta-data/",
] as const;

const STOP_DEMOS: StopDemo[] = FUNNEL_DEMOS.filter((d) => d.id !== "once-or-for-good").map((d) => ({
  ...d,
  cmds: d.id === "lines-that-cant-be-crossed" ? LINES_CMDS : d.cmds,
}));

/**
 * A chapter card that is NOT spoken.
 *
 * Series house style: the outro ends on a title card the narrator does not
 * read, and overlay.ts's chapter() always speaks what it renders. Rather than
 * widen a module every other video in the series imports, drive the same
 * overlay primitive directly for this one card — the same pattern
 * 01-getting-started.spec.ts uses for its own outro.
 */
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
  await page.waitForTimeout(400);
}

// ---------------------------------------------------------------------------
// Act 1 — back into the funnel
// ---------------------------------------------------------------------------

test("V03 act 1 — open on the demos", async () => {
  test.setTimeout(120_000);
  const page = stage();
  // Owner call (2026-08-23, superseding the same-day ffwd version): the film
  // OPENS on the demos — no funnel re-entry, no fast-forward blur. /demos
  // redirects straight into the funnel's first demo step (App.tsx), which
  // renders the identical DemoDetail markup the old catalog card did — same
  // DemoRunControls, same testids/headings — episode 10 films here for
  // exactly this reason: it needs no wizard state to reach.
  await page.goto("/setup?step=sealed-box");
  await page.bringToFront();

  // Fail here rather than minutes into a silent, caption-less take — the same
  // guard 01's own first test opens with, and just as true here: this file
  // gets its OWN fresh browser (stage.ts registers one beforeAll per spec
  // file), so nothing upstream has already proven the overlay installed.
  await expect
    .poll(() => page.evaluate(() => typeof (window as unknown as Record<string, unknown>).__demo), { timeout: 15_000 })
    .toBe("object");

  await expect(page.getByRole("heading", { name: "The sealed box", level: 2 })).toBeVisible({ timeout: 30_000 });
});

// ---------------------------------------------------------------------------
// Act 2 — the four demos. Every wait in this loop is load-bearing and every
// one of them is moved verbatim from 01-getting-started.spec.ts's pre-split
// act 3. Read the comments before touching the arithmetic: each one records a
// specific way a take went green while the terminal on screen showed the
// opposite.
// ---------------------------------------------------------------------------

test("V03 act 2 — four ways the boundary holds", async () => {
  test.setTimeout(1_200_000);
  const page = stage();

  await chapter(page, "What it stops", "Four boundary behaviors, proved on camera");
  await caption(page, "Setup is one thing.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Now let's see the boundary actually work.");
  await beat(page, PACE.read);
  await caption(page, "Four small tests.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Four real sandboxes.");
  await beat(page, BEAT_SHORT);
  await caption(page, "And every decision is visible on screen.");
  await beat(page, PACE.read);

  for (const demo of STOP_DEMOS) {
    await expect(page.getByRole("heading", { name: demo.label, level: 2 })).toBeVisible({ timeout: 60_000 });

    // Ring the demo's own card as its intro speaks — a silent visual; the
    // owner's plain-language lines below already say what changed. (The old
    // anchor here, demo-policy-<id>, only ever existed on the FUNNEL's step
    // pages — /demos' catalog card has no YAML block, and every take since the
    // cold-open rework silently died on this ring: Playwright reported the
    // failure, the wrapper still published a truncated cut. Ring what the
    // catalog actually renders.)
    const card = page.getByTestId(`demo-card-${demo.id}`);
    await card.scrollIntoViewIfNeeded().catch(() => {});
    await spotlight(page, demo.id === "fail-then-approve" ? card.getByTestId("demo-steps") : card);

    // P2 (dialog review, owner-ratified 2026-08-23): the cold open promises
    // "Four small tests" and then never numbers them. Each test now says which
    // one it is, in the conclusion's own four words. Anchored per-BRANCH, and
    // the branch order here IS the running order: STOP_DEMOS is FUNNEL_DEMOS
    // (task.ts) minus once-or-for-good, i.e. demo-catalog.ts's order —
    // sealed-box, fail-then-approve, held-at-the-door, lines-that-cant-be-
    // crossed. Test four's label is NOT in the fourth iteration: that test
    // opens on the wikipedia deny at the TAIL of held-at-the-door (see the
    // file header), so the label rides there.
    if (demo.id === "sealed-box") {
      await caption(page, "Test one: denied.");
      await beat(page, BEAT_SHORT);
      await caption(page, "First, something the policy simply doesn't allow.");
      await beat(page, PACE.read);
    } else if (demo.id === "fail-then-approve") {
      await caption(page, "Test two: denied, but it can ask.");
      await beat(page, BEAT_SHORT);
      await caption(page, "Now we'll give the policy a different instruction.");
      await beat(page, PACE.read);
      await caption(page, "Don't silently refuse.");
      await beat(page, BEAT_SHORT);
      await caption(page, "Ask me.");
      await beat(page, BEAT_SHORT);
    } else if (demo.id === "held-at-the-door") {
      await caption(page, "Test three: held for a decision.");
      await beat(page, BEAT_SHORT);
      await caption(page, "This time, the request is already in progress when the policy stops it.");
      await beat(page, PACE.read);
    }
    // lines-that-cant-be-crossed: no intro of its own — the owner's script
    // picks this demo back up mid-scene (see the held-at-the-door block
    // below, which carries this test's opening lines).
    await spotlight(page, null);

    // No caption on the click — "starting the sandbox" narrates itself.
    await act(page, page.getByTestId(`demo-start-${demo.id}`));
    await expect(page.locator(".xterm-screen").first()).toBeVisible({ timeout: SANDBOX_UP });
    await beat(page, PACE.read);

    for (const [i, cmd] of demo.cmds.entries()) {
      // fail-then-approve and held-at-the-door both run under an approve
      // policy; only fail-then-approve repeats the SAME command a second time
      // (the first is refused and raises the approval, the retry after
      // approval is what gets through) — held-at-the-door's single command
      // is decided live, below, while it is still hanging.
      if (demo.approve && i === 1) {
        await decide(page, "Approve", "Approve it.", "example.com", demo.scope);
        if (demo.id === "fail-then-approve") {
          await caption(page, "Now run the same command again.");
          await beat(page, BEAT_SHORT);
        }
      }
      // Owner note (2026-08-21): the first attempt used to fail MUTE and
      // "Approve it." landed from nowhere — walk the viewer through the
      // fail-then-approve arc at each visible moment.
      if (demo.id === "fail-then-approve" && i === 0) {
        await caption(page, "This command tries to get out.");
        await beat(page, BEAT_SHORT);
      }
      await typeInTerminal(page, cmd);
      // Let the command actually RESOLVE before moving on. This is not pacing:
      // ending a demo while a curl is still in flight kills the sandbox before
      // its decision reaches the audit log, and the proof silently goes missing
      // from a take that looks perfectly fine. Derive the wait from the
      // command's own --max-time. Demos that involve an approval are excluded —
      // their curl is SUPPOSED to still be hanging when we decide it.
      const maxTime = demo.approve ? null : cmd.match(/--max-time (\d+)/);
      await beat(page, maxTime ? (Number(maxTime[1]) + 2) * 1000 : PACE.read + 800);

      // The first attempt's on-screen refusal, narrated while it is visible —
      // the raised approval row is what "a question waiting for us" points at,
      // and decide()'s own ring lands on that row seconds later.
      if (demo.id === "fail-then-approve" && i === 0) {
        await caption(page, "The first time, it fails.");
        await beat(page, BEAT_SHORT);
        await caption(page, "But now there's a question waiting for us.");
        await beat(page, PACE.read);
      }

      // The retry after an approval MUST visibly succeed. This is the payoff of
      // fail-then-approve, and without the assertion a failed retry just raises
      // a fresh approval that the NEXT step latches onto — so the take stays
      // green while narrating success over a terminal showing two refusals and
      // no success. (The proxy itself warns a `once` grant is spent before
      // success is guaranteed.)
      if (demo.approve && i === 1) {
        const term = page.locator(".xterm-screen").first();
        await centerInFrame(term);
        await expect(term).toContainText(/HTTP\/2 200|HTTP\/1\.1 200/, { timeout: 45_000 });
        if (demo.id === "fail-then-approve") {
          await caption(page, "The command continues.");
          await beat(page, PACE.read);
          await caption(page, "Same request.");
          await beat(page, BEAT_SHORT);
          await caption(page, "Same sandbox.");
          await beat(page, BEAT_SHORT);
          await caption(page, "The only thing that changed was the decision.");
          await beat(page, PACE.read);
        }
      }
    }

    if (demo.id === "sealed-box") {
      // "Instant refusal on camera" is the whole beat, so prove the refusal is
      // on camera. This policy is always_deny with an empty allowlist: the
      // proxy answers the CONNECT with a 403 and curl reports it. A take that
      // somehow filmed a 200 here would narrate "deny is the default" over a
      // successful request.
      const term = page.locator(".xterm-screen").first();
      await centerInFrame(term);
      await expect(term).toContainText(/403|curl: \(\d+\)/, { timeout: 45_000 });
      // sealed-box's policy never asks a human, so there is no decide() to
      // attach the owner's SAY-ON-DECIDE line to — it is spoken here, at the
      // moment the refusal actually lands on screen.
      await caption(page, "The request is denied.");
      await beat(page, BEAT_SHORT);
      await caption(page, "The sandbox asked.");
      await beat(page, BEAT_SHORT);
      await caption(page, "The proxy said no.");
      await beat(page, BEAT_SHORT);
      await caption(page, "And the command gets a normal refusal.");
      await beat(page, PACE.read);
    }

    if (demo.approve && demo.cmds.length === 1) {
      // held-at-the-door: the curl is still hanging at the proxy right now.
      await caption(page, "Notice what's happening.");
      await beat(page, PACE.read);
      await caption(page, "The command hasn't failed.");
      await beat(page, BEAT_SHORT);
      await caption(page, "It's waiting.");
      await beat(page, BEAT_SHORT);
      await caption(page, "Nothing has been sent through the door yet.");
      await beat(page, PACE.read);
      await decide(page, "Approve", "Approve it.", "example.com");
      await caption(page, "And now the request completes.");
      await beat(page, PACE.read);
      await caption(page, "No retry.");
      await beat(page, BEAT_SHORT);
      await caption(page, "It was already waiting at the boundary.");
      await beat(page, PACE.read);

      // The owner's fourth test begins here, still inside this same sandbox —
      // the product's own "held-at-the-door" demo card carries the
      // wikipedia+Deny step as its own third step (demo-catalog.ts), so this
      // test rides it rather than getting a sandbox of its own.
      await typeInTerminal(page, "curl -sSI --max-time 60 https://wikipedia.org");
      await beat(page, 1200);
      await caption(page, "Test four: walled off.");
      await beat(page, BEAT_SHORT);
      await caption(page, "Now another ordinary host.");
      await beat(page, BEAT_SHORT);
      await caption(page, "This one should be refused outright.");
      await beat(page, PACE.read);
      await decide(page, "Deny", "Deny.", "wikipedia.org");
      await expect(page.getByTestId("demo-audit-panel")).toContainText(/wikipedia/, { timeout: 30_000 });
      await spotlight(page, page.getByTestId("demo-audit-panel"));
      await caption(page, "The refusal lands beside the approval in the record.");
      await beat(page, PACE.read);
      await caption(page, "And then there's the address you really don't want an arbitrary workload reaching:");
      await beat(page, PACE.read);
      await caption(page, "the cloud metadata service.");
      await beat(page, BEAT_SHORT);
      await caption(page, "That's where a cloud machine's own credentials can live.");
      await beat(page, PACE.read);
      await spotlight(page, null);
    }

    if (demo.id === "lines-that-cant-be-crossed") {
      // Prove the block from the TERMINAL, not the audit panel.
      //
      // SV3 corrected this beat against the live stack: the sandbox's NO_PROXY
      // covers only the proxy and localhost, so the http:// metadata probe DOES
      // reach the egress proxy, which refuses to dial it — it films as
      // `HTTP/1.1 403`, not as a bare connect failure. On a stack where the
      // proxy is out of that path it dies at the network layer instead (curl 7,
      // no route). Either shape is the same fact and both are accepted; what is
      // NOT accepted is a success.
      const term = page.locator(".xterm-screen").first();
      await expect(term).toContainText(
        /HTTP\/1\.1 403|Failed to connect to 169\.254\.169\.254|curl: \(\d+\)/,
        { timeout: 60_000 },
      );
      await centerInFrame(term);
      await spotlight(page, term);
      await caption(page, "Here, the request doesn't even get a chance to connect.");
      await beat(page, PACE.read);
      await caption(page, "The proxy refuses it.");
      await beat(page, BEAT_SHORT);
      await caption(page, "The kernel has no route there either.");
      await beat(page, BEAT_SHORT);
      await spotlight(page, null);

      // The second half of the property, and the part that separates this demo
      // from every other one in the act: there is no pending decision, because
      // there was never a decision to make. Assert the strip really is empty —
      // narrating "no approval was raised" over a pending row would invert the
      // lesson.
      await expect(page.getByTestId("live-approval-row")).toHaveCount(0);
      await caption(page, "And importantly, there's no approval prompt.");
      await beat(page, PACE.read);
      await caption(page, "Some destinations aren't merely disallowed.");
      await beat(page, BEAT_SHORT);
      await caption(page, "They're outside the set of things a human can approve.");
      await beat(page, PACE.read + 900);
    }

    const endDemo = page.getByRole("button", { name: "End demo" });
    if (await endDemo.isVisible().catch(() => false)) await act(page, endDemo);
    // Each demo is its own funnel STEP now (the redirect lands inside Getting
    // Started, not a one-page catalog) — the next iteration's heading only
    // appears after Next carries the rail forward. The quartet is consecutive
    // in catalog order (demo-catalog.ts), so this never has to skip past an
    // interleaved agent-in-the-box/record-a-policy step.
    await advance();
  }
});

// ---------------------------------------------------------------------------
// Conclusion — hands off to the interactive-agent episode (currently still
// 04-interactive-runs.spec.ts; the renumber that moves it is a separate,
// later commit).
// ---------------------------------------------------------------------------

test("V03 act 3 — conclusion", async () => {
  test.setTimeout(60_000);
  const page = stage();

  await chapter(page, "What you just saw", "Four boundary behaviors, proved on camera");
  await caption(page, "We saw four different kinds of boundary behavior.");
  await beat(page, PACE.read);
  await caption(page, "Denied.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Denied with a way to ask.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Held for a decision.");
  await beat(page, BEAT_SHORT);
  await caption(page, "And completely walled off.");
  await beat(page, BEAT_SHORT);
  await caption(page, "None of those were screenshots.");
  await beat(page, PACE.read);
  await caption(page, "They were real requests, inside real sandboxes, under real policies.");
  await beat(page, PACE.read);
  await caption(page, "And we're ready to give it some actual work.");
  await beat(page, PACE.read);
  await caption(page, "");
  await silentCard(page, "Next — 04: Add a workspace");
  await caption(page, "Next, we'll give a run something real to work on.");
  await beat(page, PACE.read);
  await caption(page, "A workspace.");
  await beat(page, BEAT_SHORT + 400);
  await caption(page, "");
});
