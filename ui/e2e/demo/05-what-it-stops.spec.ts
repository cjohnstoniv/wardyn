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

test.describe.configure({ mode: "serial" });

// ---------------------------------------------------------------------------
// The four guardrail demos, as this episode films them.
//
// FUNNEL_DEMOS (task.ts) is the shared definition — same ids, same commands,
// same captions the walkthrough proved. once-or-for-good is filtered out: its
// caret-scope beat belongs to the approval-scopes episode (old 07), which
// owns that whole subject. Two deltas on the survivors, both carried over
// from the pre-split V01 script's "Pacing trims banked" note, live here
// rather than in task.ts so walkthrough.spec.ts and the other videos are
// untouched:
//
//   - `property`: the single line each demo's own beat speaks over it — the
//     one thing that demo exists to teach. Verbatim from the V01 script
//     (capitalised: the script quotes them mid-sentence, and
//     scripts/narrate-prewarm.sh only warms lines that start with a capital).
//   - `lines-that-cant-be-crossed` loses its third command and shortens the
//     second. The 192.168.1.1 probe teaches nothing the metadata probe did not
//     already teach, and --max-time 5 twice is dead air this episode does not
//     need either.
// ---------------------------------------------------------------------------

type StopDemo = {
  id: string;
  label: string;
  cmds: readonly string[];
  caption: string;
  approve: boolean;
  scope: "run" | "once";
  intro: string;
  policyLine: string;
  property: string;
};

const DEMO_PROPERTY: Record<string, string> = {
  "sealed-box": "Deny is the default, and a refusal needs no human.",
  "fail-then-approve": "A refusal can also ask — the boundary moves when a person moves it.",
  "held-at-the-door": "The decision can happen while the request is still open.",
  "lines-that-cant-be-crossed": "Some limits are not policy at all. No setting can open them.",
};

// What each demo IS, spoken over its heading before anything runs — and the
// policy it launches under, spoken over the step's own "The policy Wardyn
// runs" block. The intro says what to watch for; the policy line reads the
// config the viewer can see, in plain words. Owner direction (2026-08-17):
// every demo gets a real introduction including its initial policy, not just
// the property line.
const DEMO_INTRO: Record<string, string> = {
  "sealed-box": "First, the sealed box — a sandbox whose policy allows nothing at all.",
  "fail-then-approve": "Next, the same refusal — but this policy asks a person instead of just saying no.",
  "held-at-the-door": "Now the live version: the request is held open while Wardyn waits for your answer.",
  "lines-that-cant-be-crossed": "Then the opposite extreme — a policy that opens the whole public internet.",
};
const DEMO_POLICY_LINE: Record<string, string> = {
  "sealed-box": "Here is its whole policy: an empty allow list, and unlisted traffic denied outright.",
  "fail-then-approve": "One field changed — an unlisted host now raises an approval instead of a flat no.",
  "held-at-the-door": "Same shape, but held: off-policy traffic parks at the proxy until you decide it.",
  "lines-that-cant-be-crossed": "Allow-all egress — the loosest policy Wardyn will write.",
};

/** Same two beats as the card's first two steps, minus the LAN probe. */
const LINES_CMDS = [
  "curl -sSI https://example.com",
  "curl -sSI --max-time 2 http://169.254.169.254/latest/meta-data/",
] as const;

const STOP_DEMOS: StopDemo[] = FUNNEL_DEMOS.filter((d) => d.id !== "once-or-for-good").map((d) => ({
  ...d,
  cmds: d.id === "lines-that-cant-be-crossed" ? LINES_CMDS : d.cmds,
  property: DEMO_PROPERTY[d.id],
  intro: DEMO_INTRO[d.id],
  policyLine: DEMO_POLICY_LINE[d.id],
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

test("V05a act 1 — back to the funnel", async () => {
  test.setTimeout(240_000);
  const page = stage();
  await page.goto("/setup");
  await page.bringToFront();

  // Fail here rather than minutes into a silent, caption-less take — the same
  // guard 01's own first test opens with, and just as true here: this file
  // gets its OWN fresh browser (stage.ts registers one beforeAll per spec
  // file), so nothing upstream has already proven the overlay installed.
  await expect
    .poll(() => page.evaluate(() => typeof (window as unknown as Record<string, unknown>).__demo), { timeout: 15_000 })
    .toBe("object");

  // A fresh browser session starts the funnel over at step 1. The barrier
  // pick and Secrets carry over from this host's own state (video two proved
  // them); Network does not — steps.ts's CorpNetworkState is deliberately
  // SESSION-only ("never a stale 'reached' surviving a page reload"), so a new
  // browser has to earn it again. None of this is new content, so own it in
  // one clause (series ruling S7) and move fast rather than re-teaching it.
  await expect(page.getByRole("heading", { name: "Pick your barrier", level: 2 })).toBeVisible({ timeout: 30_000 });
  await caption(page, "Quickly, back through what video two already proved — barrier, network, secrets.");
  const tiers = page.getByRole("radiogroup", { name: "Barrier tier" });
  const fence = tiers.getByRole("radio", { name: /Fence/ });
  await expect(fence).toBeEnabled();
  await act(page, fence);
  await expect(fence).toHaveAttribute("aria-checked", "true");
  await advance();

  await expect(page.getByRole("heading", { name: "Network", level: 2 })).toBeVisible({ timeout: 30_000 });
  await act(page, page.getByRole("button", { name: "Test connectivity" }));
  await expect(page.getByText(/^Reached · direct/).first()).toBeVisible({ timeout: SANDBOX_UP });
  await advance();

  await expect(page.getByRole("heading", { name: "Secrets", level: 2 })).toBeVisible({ timeout: 30_000 });
  await advance();

  await expect(page.getByRole("heading", { name: "The sealed box", level: 2 })).toBeVisible({ timeout: 30_000 });
});

// ---------------------------------------------------------------------------
// Act 2 — the four demos. Every wait in this loop is load-bearing and every
// one of them is moved verbatim from 01-getting-started.spec.ts's pre-split
// act 3. Read the comments before touching the arithmetic: each one records a
// specific way a take went green while the terminal on screen showed the
// opposite.
// ---------------------------------------------------------------------------

test("V05a act 2 — four ways the boundary holds", async () => {
  test.setTimeout(1_200_000);
  const page = stage();

  await caption(page, "Setup is done. Now watch what it actually stops.");
  await beat(page, PACE.read);
  await chapter(page, "What it stops", "Four sandboxes, refusals proved on camera");
  await caption(page, "Four small demos, each a real sandbox under a policy you can read on screen.");
  await beat(page, PACE.read);
  await caption(page, "Together they show how a sandbox's access is configured, tuned, and enforced.");
  await beat(page, PACE.read);

  // See the audit-panel block at the bottom of the loop: the trail line is a
  // fact about Wardyn, said once, not a per-demo refrain.
  let auditLineSpoken = false;

  for (const demo of STOP_DEMOS) {
    await expect(page.getByRole("heading", { name: demo.label, level: 2 })).toBeVisible({ timeout: 60_000 });

    // Owner note (2026-08-17): each demo gets a real introduction — what it is
    // and what to watch for — not just the property line. The intro is spoken
    // over the step's own heading and overview text, which the camera is
    // already looking at.
    await caption(page, demo.intro);
    await beat(page, PACE.read);

    // ...and the POLICY the sandbox is about to launch under. The step body
    // renders it in full ("The policy Wardyn runs", demos-step.tsx) — this
    // series claims policies are readable, so read one, on camera, every time.
    const policyBlock = page.getByTestId(`demo-policy-${demo.id}`);
    await policyBlock.scrollIntoViewIfNeeded().catch(() => {});
    if (demo.id === "fail-then-approve") {
      // [a] ring the one line that changed from the sealed box's policy, not
      // the whole block (ADJUDICATION V01, series ruling S3).
      await spotlight(page, policyBlock.getByText(/first_use_approval/));
    } else {
      await spotlight(page, policyBlock);
    }
    await caption(page, demo.policyLine);
    await beat(page, PACE.read + 600);
    if (demo.id === "sealed-box") {
      // [a] CC1 and the 900s auto-stop sit on this same policy screen and went
      // unexplained (ADJUDICATION V01) — said once, the first time any policy
      // is shown.
      await caption(page, "The other two lines: CC1 is Fence by its policy name, and the box kills itself in fifteen minutes.");
      await beat(page, PACE.read);
    }
    await spotlight(page, null);

    // No caption on the click — "starting the sandbox" narrates itself.
    await act(page, page.getByTestId(`demo-start-${demo.id}`));
    await expect(page.locator(".xterm-screen").first()).toBeVisible({ timeout: SANDBOX_UP });
    await beat(page, PACE.read);

    // The one property this demo exists to teach, spoken over its sandbox
    // coming up (~12s of boot the script already budgeted as dead air).
    await caption(page, demo.property);
    await beat(page, PACE.read + 600);

    for (const [i, cmd] of demo.cmds.entries()) {
      // fail-then-approve and held-at-the-door both run under an approve
      // policy; only fail-then-approve repeats the SAME command a second time
      // (the first is refused and raises the approval, the retry after
      // approval is what gets through) — held-at-the-door's single command
      // is decided live, below, while it is still hanging.
      if (demo.approve && i === 1) {
        await decide(
          page,
          "Approve",
          "The refusal raised an approval. Granting it — then the very same command again.",
          "example.com",
          demo.scope,
        );
        // Every remaining approve-demo grants "This run" — once-or-for-good
        // (the one that needed Once) moved to the approval-scopes episode.
        await caption(
          page,
          "A plain Approve keeps it allowed for the rest of this run (the split button's caret offers other options).",
        );
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

      // The retry after an approval MUST visibly succeed. This is the payoff of
      // fail-then-approve, and without the assertion a failed retry just raises
      // a fresh approval that the NEXT step latches onto — so the take stays
      // green while narrating "approved, and it goes through" over a terminal
      // showing two refusals and no success. (The proxy itself warns a `once`
      // grant is spent before success is guaranteed.)
      if (demo.approve && i === 1) {
        // [a] this receipt used to sit under the caption bar (ADJUDICATION
        // V01, series ruling S2) — center it before asserting.
        const term = page.locator(".xterm-screen").first();
        await centerInFrame(term);
        await expect(term).toContainText(/HTTP\/2 200|HTTP\/1\.1 200/, { timeout: 45_000 });
        if (demo.id === "fail-then-approve") {
          // [a] this demo's best, unspoken property — an agent inside the box
          // cannot tell a flat no from a held ask (ADJUDICATION V01).
          await caption(page, "From inside, both denials look identical — the agent never knows a human was asked.");
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
      // [a] this receipt also sat under the bar (ADJUDICATION V01, S2), and
      // the payoff itself went unread over an 8s silent gap (ADJUDICATION V01).
      await centerInFrame(term);
      await expect(term).toContainText(/403|curl: \(\d+\)/, { timeout: 45_000 });
      await caption(page, "Four-oh-three, tunnel refused — the ask reached the proxy, and the proxy said no.");
      await beat(page, PACE.read);
    }

    if (demo.approve && demo.cmds.length === 1) {
      // held-at-the-door: the curl is still hanging at the proxy right now.
      await decide(page, "Approve", "The command has not failed — it is hanging, held open at the proxy, waiting for a human.", "example.com");
      await caption(page, "Approved inside the window, so that same in-flight request completes. No retry.");
      await beat(page, PACE.read + 900);

      // ...and the other half of a live decision: a host you refuse.
      await typeInTerminal(page, "curl -sSI --max-time 60 https://wikipedia.org");
      await beat(page, 1200);
      await decide(page, "Deny", "A second host, held the same way — this one gets refused.", "wikipedia.org");
      await beat(page, PACE.read);
      // [a] the deny used to end the beat right here, never shown landing in
      // the trail (ADJUDICATION V01).
      await expect(page.getByTestId("demo-audit-panel")).toContainText(/wikipedia/, { timeout: 30_000 });
      await spotlight(page, page.getByTestId("demo-audit-panel"));
      await caption(page, "And the refusal is on the record beside the allow — deny, wikipedia, just now.");
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
      // [a] the metadata beat: the strongest claim in the video, with no
      // receipt and unglossed jargon (ADJUDICATION V01) — center+spotlight the
      // refusal before speaking over it, then the plain-language line the
      // product's own small text already carries.
      await centerInFrame(term);
      await spotlight(page, term);
      await caption(page, "The proxy refuses to even dial it — and the kernel has no route there either.");
      await beat(page, PACE.read + 900);
      await caption(page, "That address is where a cloud machine's own credentials live — the classic theft target.");
      await beat(page, PACE.read);

      // The second half of the property, and the part that separates this demo
      // from every other one in the act: there is no pending decision, because
      // there was never a decision to make. Assert the strip really is empty —
      // narrating "no approval was raised" over a pending row would invert the
      // lesson.
      await expect(page.getByTestId("live-approval-row")).toHaveCount(0);
      await caption(page, "No approval was raised, because no approval could have granted it.");
      await beat(page, PACE.read + 900);
      // [a] the third metadata edit: the trail staying empty too is shown,
      // not left to another silent gap (ADJUDICATION V01).
      await spotlight(page, page.getByTestId("demo-audit-panel"));
      await caption(page, "Nothing new lands in the trail either — no deny, no ask. There was never a decision to make.");
      await beat(page, PACE.read);
      await spotlight(page, null);
    }

    // The decisions are on the record before we move on. Deliberately skipped
    // for lines-that-cant-be-crossed: its headline denial never reaches the
    // proxy as a DENY (see above), so its panel shows only the allow — and
    // narrating "every one of those decisions is on the record" over that would
    // be a claim the trail does not support.
    const auditPanel = page.getByTestId("demo-audit-panel");
    if (demo.id !== "lines-that-cant-be-crossed" && (await auditPanel.isVisible().catch(() => false))) {
      await spotlight(page, auditPanel);
      // Spoken ONCE, on the first demo that has a panel to show. It is a fact
      // about the product, not about this demo, so saying it over all four cost
      // ~39 words and taught nothing after the first time — the spotlight alone
      // carries it thereafter, which is what a presenter would actually do.
      if (!auditLineSpoken) {
        auditLineSpoken = true;
        await caption(page, "Every one of those decisions landed in the audit trail, live, as it happened.");
      }
      await beat(page, PACE.read);
      await spotlight(page, null);
    }

    const endDemo = page.getByRole("button", { name: "End demo" });
    if (await endDemo.isVisible().catch(() => false)) await act(page, endDemo);
    await advance();
  }
});

// ---------------------------------------------------------------------------
// Conclusion — hands off to the interactive-agent episode (currently still
// 04-interactive-runs.spec.ts; the renumber that moves it is a separate,
// later commit).
// ---------------------------------------------------------------------------

test("V05a act 3 — conclusion", async () => {
  test.setTimeout(60_000);
  const page = stage();

  await chapter(page, "What you just saw", "Four refusals, none of them staged to fail politely");
  await caption(page, "Denied outright, denied with a way to ask, held for a live decision, and walled off entirely.");
  await beat(page, PACE.read);
  await caption(page, "Every one of those was a real sandbox, under a real policy, doing exactly what it was told.");
  await beat(page, PACE.read);
  await caption(page, "Next: hand one of these boxes a real job, and watch it live, interactively.");
  await beat(page, PACE.read);
  await caption(page, "");
  await silentCard(page, "Next — 06: Interactive runs");
});
