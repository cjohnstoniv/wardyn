/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * Video 01 of the 0.5 series — "Getting started".
 *
 * WHAT THIS FILMS. One command has already brought a control plane up on this
 * machine; nothing has been configured and nothing has run. The take opens on
 * the first-boot hero, walks the funnel's three setup steps — the barrier, the
 * mandatory Network gate, Secrets — then the five hands-on guardrail demos,
 * and ENDS THERE, on a spoken conclusion. No workspace is onboarded and no run
 * is launched: adding a workspace is video 02 and running one is video 03
 * (owner restructure, 2026-08-17), so this video closes on its own subject —
 * a governed host whose guardrails you just watched hold.
 *
 * WHERE THE CODE CAME FROM. This is an extraction of walkthrough.spec.ts acts
 * 1-4, and the choreography is MOVED, not rewritten: the funnel waits, the
 * per-demo pacing, the "let the curl actually resolve" arithmetic and the
 * post-approval payoff assertions are all proven on camera and a rewrite would
 * lose them. What changed is the narration — every caption below is a SAY line
 * from the V01 presenter script (the plan file's "### V01 — Getting started"),
 * verbatim, because those lines were budgeted at ~2.4 words/sec and the
 * captions ARE what Kokoro speaks.
 *
 * This is NOT a test. It asserts only enough to keep itself honest and to know
 * when to advance; a failure here means the recording is wrong, not that the
 * product is broken. It runs against the REAL compose stack on :8080 with real
 * sandboxes — the hermetic `-runner none` e2e backend cannot start one at all.
 *
 * STAGING THE OPERATOR OWNS (off camera, before the take rolls):
 *   1. FULL RESET. `WARDYN_FORCE_RESET=1 ./scripts/up.sh reset-all --purge-env`
 *      (never --purge-images), then a containerized `make setup`. V01 is the
 *      ONE video in the series that resets — every later take runs
 *      `record-demo.sh --no-reset` (SV20).
 *   2. SERIES SSH BRING-UP, before `make setup` (SV4/DA9/DA15/DA16):
 *      `export WARDYN_SSH_LISTEN=:2222 WARDYN_SSH_ADVERTISE=127.0.0.1:2222`.
 *      Doing it later regenerates the host key mid-series and V10 films an
 *      on-camera HOST-IDENTIFICATION-CHANGED.
 *   3. MODEL PRE-CONNECTED off camera, via token-stdin, on the MANAGED
 *      subscription lane. Beat 4 admits this aloud, and asserts it on screen:
 *      an unconnected lane makes that SAY line a lie over a screen that
 *      contradicts it.
 *   4. FRESH BROWSER PROFILE. The hero only renders while
 *      `wardyn-onboarding-seen` is unset; stage.ts builds a new context per
 *      take, so this is satisfied as long as DEMO_CDP is NOT pointed at a
 *      browser that has already seen it.
 *   5. LOCAL MODE, no auth (SV1) — never seed WARDYN_DEMO_TOKEN for this take.
 *      A login wall in front of the hero is not what video one opens on.
 *      (The narration deliberately does NOT sell "local, not cloud": that is a
 *      deployment detail, and the opening's job is to say what Wardyn IS.)
 *
 * Selectors are getByRole + accessible names, matching ui/e2e/fixtures.ts and
 * the rest of the suite: a copy change breaks this loudly and in one place,
 * markup churn does not break it at all.
 */

import { test, expect, type Page } from "@playwright/test";
import { FUNNEL_DEMOS } from "./task";
import { act, beat, caption, chapter, PACE, spotlight, typeInTerminal } from "./overlay";
// The browser, the recorded context and the shared page live in stage.ts:
// importing it is what registers this file's beforeAll/afterAll, and each act
// reads the page out of stage() rather than closing over a module-level `let`.
import { stage } from "./stage";
import { advance, APPROVAL_APPEARS, decide } from "./funnel";

test.skip(!process.env.WARDYN_DEMO, "demo recording — run via `make record-demo` (exports WARDYN_DEMO=1)");

// Sandboxes are real containers — the connectivity probe and every one of the
// five demos launches one. These are minutes, not seconds. They are ceilings
// for waiting on the PRODUCT; the pacing the viewer sees comes from overlay.ts.
// (APPROVAL_APPEARS is the third of them and lives in funnel.ts, beside the
// decide() that waits on it.)
const SANDBOX_UP = 180_000;

test.describe.configure({ mode: "serial" });

// ---------------------------------------------------------------------------
// The five guardrail demos, as V01 films them.
//
// FUNNEL_DEMOS (task.ts) is the shared definition — same ids, same commands,
// same captions the walkthrough proved. Two V01-only deltas, both from the
// script's "Pacing trims banked" note, live here rather than in task.ts so
// walkthrough.spec.ts and the other nine videos are untouched:
//
//   - `property`: the single line B6-B10 speak over each demo — the one thing
//     that demo exists to teach. Verbatim from the V01 script (capitalised: the
//     script quotes them mid-sentence, and scripts/narrate-prewarm.sh only
//     warms lines that start with a capital).
//   - `lines-that-cant-be-crossed` loses its third command and shortens the
//     second. The 192.168.1.1 probe teaches nothing the metadata probe did not
//     already teach, and --max-time 5 twice is ~10s of dead air in the longest
//     video of the series.
// ---------------------------------------------------------------------------

type V01Demo = {
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
  "once-or-for-good": "An approval has a scope, and the narrowest is one connection.",
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
  "once-or-for-good": "Last, scopes: when you do say yes, how far should that yes reach?",
};
const DEMO_POLICY_LINE: Record<string, string> = {
  "sealed-box": "Here is its whole policy: an empty allow list, and unlisted traffic denied outright.",
  "fail-then-approve": "One field changed — an unlisted host now raises an approval instead of a flat no.",
  "held-at-the-door": "Same shape, but held: off-policy traffic parks at the proxy until you decide it.",
  "lines-that-cant-be-crossed": "Allow-all egress — the loosest policy Wardyn will write.",
  "once-or-for-good": "The ask-first policy again. What changes this time is how we answer it.",
};

/** Same two beats as the card's first two steps, minus the LAN probe. */
const LINES_CMDS = [
  "curl -sSI https://example.com",
  "curl -sSI --max-time 2 http://169.254.169.254/latest/meta-data/",
] as const;

const V01_DEMOS: V01Demo[] = FUNNEL_DEMOS.map((d) => ({
  ...d,
  cmds: d.id === "lines-that-cant-be-crossed" ? LINES_CMDS : d.cmds,
  property: DEMO_PROPERTY[d.id],
  intro: DEMO_INTRO[d.id],
  policyLine: DEMO_POLICY_LINE[d.id],
}));

/**
 * A chapter card that is NOT spoken.
 *
 * The script's outro ends on a title card the narrator deliberately does not
 * read ("card (caption-only, NOT spoken)"), and overlay.ts's chapter() always
 * speaks what it renders. Rather than widen a module every other video in the
 * series imports, drive the same overlay primitive directly for this one card.
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
// Cold open + B1 — first light
// ---------------------------------------------------------------------------

test("V01 act 1 — cold open, first light", async () => {
  test.setTimeout(180_000);
  const page = stage();
  await page.goto("/");
  await page.bringToFront();

  // Fail here rather than six minutes into a silent, caption-less take: every
  // narration call degrades to a no-op by design, so nothing downstream would
  // ever complain about an overlay that failed to install.
  await expect
    .poll(() => page.evaluate(() => typeof (window as unknown as Record<string, unknown>).__demo), { timeout: 15_000 })
    .toBe("object");

  // A fresh install has no runs and no dismissed tour, so firstRunLanding()
  // (setup-gate.ts) redirects / → /setup and the hero renders. That redirect is
  // the real first-light behaviour and worth filming — but it is conditional on
  // has_runs being false, and this video's own connectivity probe LAUNCHES A RUN
  // to prove the path. So the moment this driver has run once, `/` lands on
  // /runs instead. Fall through to /setup rather than making every iteration
  // require a full stack reset. (The hero itself only needs an unset
  // wardyn-onboarding-seen, which a fresh browser profile always gives us — and
  // if that flag IS set, this expect fails loudly, which is the correct
  // outcome: V01's cold open IS the hero.)
  const hero = page.getByRole("heading", { name: "Run anything. Keep your keys.", level: 1 });
  if (!(await hero.isVisible().catch(() => false))) {
    await page.goto("/setup");
  }
  await expect(hero).toBeVisible({ timeout: 60_000 });

  // The opening card and the three lines under it are the only chance this
  // series gets to say WHAT WARDYN IS. The first cut said "One command to a
  // governed host", which is a slogan: it describes no mechanism, names no
  // problem, and the hero already on screen behind it makes the same promise
  // better. Both halves now state the actual shape of the product — sandbox,
  // proxy, no resident credential — because a viewer who does not know what
  // this is by second twenty has no reason to watch the other nine videos.
  await chapter(page, "Wardyn", "An agent gets its own sandbox, a policy it cannot exceed, and no credentials to steal");
  await caption(page, "Wardyn runs coding agents in a sandbox with no route out except a proxy you control.");
  await beat(page, PACE.read);
  await caption(page, "It never hands them a key — the proxy attaches credentials on the way out.");
  await beat(page, PACE.read);
  await caption(page, "So a compromised agent has nothing to steal, and every attempt is on the record.");
  await beat(page, PACE.read);
  await caption(page, "This series is for whoever has to sign off on that.");
  await beat(page, PACE.read);
  await caption(page, "Video one: from nothing to a governed host, with the guardrails proved on camera.");
  await beat(page, PACE.read);

  // B1 — the hero, then the LIVE host chips under it. The chips are the honest
  // half of the cold open: they are read off this machine's real SetupStatus,
  // not a marketing screenshot, which is exactly what the SAY line claims.
  await spotlight(page, hero);
  await beat(page, PACE.read);
  await spotlight(page, page.getByText("This host right now:").locator(".."));
  await caption(page, "These are read off this machine — the barriers it can actually enforce, not a brochure.");
  await beat(page, PACE.read + 600);
  await spotlight(page, null);

  await act(page, page.getByRole("button", { name: /^Get started/ }));
  // "Step 1 of 10" is the script's own SCREEN note and Track B froze the funnel
  // STRUCTURE (DA12) before this spec was extracted. A different count means the
  // funnel gained or lost a step — and act 3 below films five of them by name,
  // so the video would be wrong in a way no caption could cover.
  await expect(page.getByText(/^Step 1 of 10$/)).toBeVisible({ timeout: 30_000 });
});

// ---------------------------------------------------------------------------
// B2-B4 — the barrier, the one gate, the model
// ---------------------------------------------------------------------------

test("V01 act 2 — barrier, network, model", async () => {
  test.setTimeout(300_000);
  const page = stage();

  // --- B2 — Pick your barrier ---------------------------------------------
  // This beat is the one place the series teaches SANDBOXING ITSELF. The
  // audience assumption for video one is someone who can run a coding agent and
  // has never had a reason to care what a kernel boundary is — so the three
  // tiers get named mechanisms (container, gVisor, Kata microVM) and, more
  // importantly, an honest sentence each about what they do NOT stop. The first
  // cut spent three compressed lines here and read as jargon to exactly the
  // person this video exists for.
  await expect(page.getByRole("heading", { name: "Pick your barrier", level: 2 })).toBeVisible({ timeout: 30_000 });
  await caption(page, "An agent that can run commands can do anything your own shell can do.");
  await beat(page, PACE.read);
  await caption(page, "A barrier is what stands between it and the rest of your machine.");
  await beat(page, PACE.read);

  const tiers = page.getByRole("radiogroup", { name: "Barrier tier" });
  await spotlight(page, tiers);
  await caption(page, "Wardyn builds three of them, and they trade speed for strength.");
  await beat(page, PACE.read);
  await caption(page, "Fence is a container — ordinary Linux isolation, sharing your machine's kernel.");
  await beat(page, PACE.read);
  await caption(page, "Fast to start, and the weakest: a kernel bug is a way out of it.");
  await beat(page, PACE.read);
  await caption(page, "Wall runs gVisor, which hands the agent a kernel written in software.");
  await beat(page, PACE.read);
  await caption(page, "Its system calls hit that copy, so an exploit has nothing real to land on.");
  await beat(page, PACE.read);
  await caption(page, "Vault is a micro virtual machine, through Kata — its own kernel, its own hardware.");
  await beat(page, PACE.read);
  await caption(page, "Strongest, slowest to boot, and it needs virtualization the host may not have.");
  await beat(page, PACE.read + 600);

  // The permanent "Doesn't stop:" row (copy.ts's RESIDUAL_PREFIX) is the whole
  // honesty claim the next line makes — a tier matrix that also prints what each
  // barrier does NOT protect against.
  await spotlight(page, page.getByText("Doesn't stop:", { exact: true }));
  await caption(page, "Every tier also prints what it does not stop. No barrier is a promise of safety.");
  await beat(page, PACE.read);
  await caption(page, "And Wardyn probes this host, so it only offers the ones it can actually enforce.");
  await beat(page, PACE.read + 600);
  await spotlight(page, null);

  // Click the READY column. On the demo host (WSL2 + Docker Desktop) that is
  // Fence and only Fence — the same fact V06's script records for the same
  // stack. A radio for a tier this host cannot build renders DISABLED, so if
  // that ever stops being true this click fails loudly instead of filming a
  // selection that did not happen.
  const fence = tiers.getByRole("radio", { name: /Fence/ });
  await expect(fence).toBeEnabled();
  await act(page, fence);
  await expect(fence).toHaveAttribute("aria-checked", "true");

  await advance();

  // --- B3 — Network, the one gate ------------------------------------------
  //
  // Renamed from "Corporate network" (steps.ts STEP_LABEL/STEP_HEADING). The
  // old name described the worst case rather than the step: it read as
  // skippable to everyone not behind a corporate proxy, when what this settles
  // — can a sandbox reach the outside world at all — is a question every
  // install has to answer. The narration leads with that, and treats proxies
  // and mirrors as the special cases they are.
  await expect(page.getByRole("heading", { name: "Network", level: 2 })).toBeVisible({ timeout: 30_000 });
  await caption(page, "Network. A sandbox is sealed off by default, so it needs a way out to the internet.");
  await beat(page, PACE.read);
  await caption(page, "This step settles that, and it is the only one that can refuse to continue.");
  await beat(page, PACE.read);
  await caption(page, "On most machines it is one click and about ten seconds.");
  await beat(page, PACE.read);

  // The gate headline the footer renders above its own reason
  // (T.GATE_HEAD_UNTESTED). It is the thing that makes this step a gate, so it
  // has to be on screen before the line that names it as one.
  const gateHead = page.getByText("Connectivity isn't proven yet");
  await expect(gateHead).toBeVisible({ timeout: 30_000 });
  await spotlight(page, gateHead);
  await beat(page, PACE.read);
  await spotlight(page, null);

  // While the gate is offering this action the step body suppresses its OWN
  // Test button (corp-network-step.tsx's hideProbeButton), so there is exactly
  // one on screen — no strict-mode ambiguity to work around.
  await act(
    page,
    page.getByRole("button", { name: "Test connectivity" }),
    "It proves it the only honest way — a real sandbox, reaching out down the path a run takes.",
  );

  // A REAL sandbox goes out and comes back. .first(): the verdict chip renders
  // in the step body AND as the rail's step badge, and they say the same thing.
  const reached = page.getByText(/^Reached · direct/).first();
  await expect(reached).toBeVisible({ timeout: SANDBOX_UP });
  await spotlight(page, reached);
  await caption(page, "Reached, direct — nothing sits between this machine and the internet.");
  await beat(page, PACE.read);
  await caption(page, "On a corporate network there usually is, and you would configure that proxy here.");
  await beat(page, PACE.read);
  await caption(page, "The probe then runs through it, so what you see proven is what a run will get.");
  await beat(page, PACE.read);
  await spotlight(page, null);

  // The second tab is the whole reason this step has tabs, and the first cut
  // never mentioned it — a locked-down network is precisely the audience that
  // needs to know Wardyn has an answer for it.
  await caption(page, "Tighter networks go further and block the public registries outright.");
  await beat(page, PACE.read);
  await caption(page, "The other tab redirects those — npm, PyPI, container images — at your internal mirrors.");
  await beat(page, PACE.read);

  // Next ×2: the first press answers by revealing the step's other tab rather
  // than moving on, which is exactly what advance() is built to absorb.
  await advance();

  // --- B4 — Secrets ---------------------------------------------------------
  //
  // Renamed from "Model & git host" (owner, 2026-08-17): every lane on this
  // step — API keys, PATs, SSH keys, App credentials, and even the login
  // flows, which capture a token — lands in the same secret store the Secrets
  // page manages. The narration teaches the CATEGORY, and it carries the
  // containment split honestly: model keys and App tokens stay outside the
  // sandbox; a PAT or SSH key enters it for the clone. That split is the card
  // footers' own text now (connection-cards.tsx), so the video and the UI
  // make the same claim.
  await expect(page.getByRole("heading", { name: "Secrets", level: 2 })).toBeVisible({ timeout: 30_000 });
  await caption(page, "Secrets. Anything a run must borrow but should never own — keys, tokens, credentials.");
  await beat(page, PACE.read);
  await caption(page, "The two most runs want are a model credential and a git credential.");
  await beat(page, PACE.read);

  // PRECONDITION, ASSERTED. The next line says the model is already connected.
  // If the off-camera token-stdin connect did not land, the lane reads
  // unconnected and the narration is a claim the screen refuses — the exact
  // green-take-over-a-failed-screen failure this series has shipped before.
  const subLane = page.getByRole("radio", { name: /Claude subscription/ });
  await expect(subLane).toContainText("Connected", { timeout: 30_000 });
  await spotlight(page, subLane);
  await caption(page, "The model was connected before recording — a subscription sign-in, captured once.");
  await beat(page, PACE.read);

  // The honesty split, told over the two cards' own footers. Model card first.
  await spotlight(page, page.getByText(/keys never enter the sandbox/i).first());
  await caption(page, "Model keys never enter the sandbox — the proxy attaches them on the way out.");
  await beat(page, PACE.read + 600);

  // Git card: the footer that says which lanes DO enter the sandbox. The
  // narration must not soften this — it is the one caveat in the credential
  // story, and hiding it would be the overclaim this campaign just fixed.
  await spotlight(page, page.getByText(/Only the GitHub App lane keeps its token/i).first());
  await caption(page, "Git is honest about its one exception.");
  await beat(page, PACE.read);
  await caption(page, "A GitHub App token is brokered at the proxy and never comes inside.");
  await beat(page, PACE.read);
  await caption(page, "A personal access token or SSH key does enter the sandbox for the clone, then is wiped.");
  await beat(page, PACE.read + 600);
  await spotlight(page, null);

  await caption(page, "Any other secret a run needs is stored the same way, and handed to runs by name.");
  await beat(page, PACE.read);

  await advance();
});

// ---------------------------------------------------------------------------
// B5-B10 — the guardrails. ~59% of this video's runtime.
//
// Every wait in this act is load-bearing and every one of them is moved
// verbatim from walkthrough.spec.ts act 3. Read the comments before touching
// the arithmetic: each one records a specific way a take went green while the
// terminal on screen showed the opposite.
// ---------------------------------------------------------------------------

test("V01 act 3 — the five guardrail demos", async () => {
  test.setTimeout(1_500_000);
  const page = stage();

  // The BRIDGE (owner note: "this is a presentation"). The first three steps
  // just ended; before any demo starts, say where we are and what the next
  // stretch of the video is for — the viewer should never have to infer the
  // itinerary from what the mouse does.
  await caption(page, "Barrier picked, network proven, secrets stored — this host can now run sandboxes.");
  await beat(page, PACE.read);
  await caption(page, "So before your own work goes in one, let's watch a few.");
  await beat(page, PACE.read);
  await chapter(page, "The guardrails", "Five sandboxes, five ways the boundary holds");
  await caption(page, "Five small demos, each a real sandbox under a policy you can read on screen.");
  await beat(page, PACE.read);
  await caption(page, "Together they show how a sandbox's access is configured, tuned, and enforced.");
  await beat(page, PACE.read);

  // See the audit-panel block at the bottom of the loop: the trail line is a
  // fact about Wardyn, said once, not a per-demo refrain.
  let auditLineSpoken = false;

  for (const demo of V01_DEMOS) {
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
    await spotlight(page, policyBlock);
    await caption(page, demo.policyLine);
    await beat(page, PACE.read + 600);
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
      // fail-then-approve and once-or-for-good both run the same command
      // twice: the first is refused and raises the approval, the retry after
      // approval is what gets through — the SCOPE granted is what differs.
      if (demo.approve && i === 1) {
        await decide(
          page,
          "Approve",
          "The refusal raised an approval. Granting it — then the very same command again.",
          "example.com",
          demo.scope,
        );
        await caption(
          page,
          demo.scope === "once"
            ? "A plain Approve would keep it allowed for the rest of this run — Once covers only this one connection."
            : "A plain Approve keeps it allowed for the rest of this run (the split button's caret offers other options).",
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
      // both approve-demos, and without the assertion a failed retry just
      // raises a fresh approval that the NEXT step latches onto — so the take
      // stays green while narrating "approved, and it goes through" over a
      // terminal showing two refusals and no success. (The proxy itself warns a
      // `once` grant is spent before success is guaranteed.)
      if (demo.approve && i === 1) {
        await expect(page.locator(".xterm-screen").first()).toContainText(/HTTP\/2 200|HTTP\/1\.1 200/, {
          timeout: 45_000,
        });
      }
    }

    if (demo.id === "sealed-box") {
      // "Instant refusal on camera" is the whole beat, so prove the refusal is
      // on camera. This policy is always_deny with an empty allowlist: the
      // proxy answers the CONNECT with a 403 and curl reports it. A take that
      // somehow filmed a 200 here would narrate "deny is the default" over a
      // successful request.
      await expect(page.locator(".xterm-screen").first()).toContainText(/403|curl: \(\d+\)/, { timeout: 45_000 });
      await beat(page, PACE.read);
    }

    if (demo.id === "once-or-for-good") {
      // Once really does mean once: the grant the retry above just spent is
      // gone, so the SAME command run a third time is refused all over again
      // and raises a brand-new approval — the entire point of this demo.
      // Deliberately left undecided: proving the re-raise appeared is the
      // beat, not deciding it a second time.
      await typeInTerminal(page, demo.cmds[0]);
      await caption(
        page,
        "Same command, one more time. Once already spent itself on the last connection — refused again, and a fresh approval appears.",
      );
      await expect(page.getByTestId("live-approval-row").filter({ hasText: "example.com" })).toBeVisible({
        timeout: APPROVAL_APPEARS,
      });
      await beat(page, PACE.read + 900);
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
      await expect(page.locator(".xterm-screen").first()).toContainText(
        /HTTP\/1\.1 403|Failed to connect to 169\.254\.169\.254|curl: \(\d+\)/,
        { timeout: 60_000 },
      );
      await caption(page, "The proxy refuses to even dial it — and the kernel has no route there either.");
      await beat(page, PACE.read + 900);

      // The second half of the property, and the part that separates this demo
      // from every other one in the act: there is no pending decision, because
      // there was never a decision to make. Assert the strip really is empty —
      // narrating "no approval was raised" over a pending row would invert the
      // lesson.
      await expect(page.getByTestId("live-approval-row")).toHaveCount(0);
      await caption(page, "No approval was raised, because no approval could have granted it.");
      await beat(page, PACE.read + 900);
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
// Conclusion — the video ends here, ON the demos, by owner direction
// (2026-08-17). The workspace onboarding and Review/Finish beats that used to
// be act 4 are not filmed in this video at all any more: video 02 is "add a
// workspace" and video 03 is "run one", so getting-started closes the moment
// its own subject — a governed host whose guardrails you just watched hold —
// is proven. What replaces the old act is a spoken summary: this is a
// presentation, and a presentation ends by telling you what you saw.
// ---------------------------------------------------------------------------

test("V01 act 4 — conclusion", async () => {
  test.setTimeout(120_000);
  const page = stage();

  await chapter(page, "What you just saw", "A governed host, and five boundaries that held");
  await caption(page, "That is Wardyn: your agents and workloads run in sandboxes, under policies you write.");
  await beat(page, PACE.read);
  await caption(page, "In one sitting this host got a barrier, a proven network path, and its secrets.");
  await beat(page, PACE.read);
  await caption(page, "Then five sandboxes showed the boundary working: denied, asked, held, and scoped.");
  await beat(page, PACE.read);
  await caption(page, "Credentials stayed out of the sandbox, and every decision landed in the audit trail.");
  await beat(page, PACE.read + 600);
  await caption(page, "Next: give a run something real to work on — a workspace.");
  await beat(page, PACE.read);
  await caption(page, "Run anything. Keep your keys.");
  await beat(page, PACE.chapter);
  await caption(page, "");
  await silentCard(page, "Next — 02: Add a workspace");
});
