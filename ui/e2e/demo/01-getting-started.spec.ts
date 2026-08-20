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
 * mandatory Network gate, Secrets — then closes on a short spoken recap and a
 * handoff to the next episode. No workspace is onboarded and no run is
 * launched: adding a workspace is video 02 and running one is video 03 (owner
 * restructure, 2026-08-17), so this video closes on its own subject — a
 * governed host, ready for real work.
 *
 * SPLIT PROVENANCE (2026-08-20, owner-approved). This video used to run
 * straight through into five hands-on guardrail demos and end there. The
 * owner-approved 12-episode restructure gives those demos their own episode —
 * "What it stops" (05a-what-it-stops.spec.ts; "05a" is an interim filename,
 * because the series renumber that claims a real 05 for it is a separate,
 * later commit) — so this file now ends where the old "guardrail demos"
 * chapter card used to begin. The outro below is new; everything before it is
 * unchanged from the pre-split take, plus the persona-adjudicated edits from
 * local/demo-review-2026-08-20/ADJUDICATION.md's "V01" section that land on
 * beats still filmed here.
 *
 * WHERE THE CODE CAME FROM. This is an extraction of walkthrough.spec.ts acts
 * 1-2 (cold open through Secrets), moved rather than rewritten: the funnel
 * waits and the connectivity-probe arithmetic are proven on camera and a
 * rewrite would lose them. What changed is the narration — every caption
 * below is either a SAY line from the V01 presenter script (the plan file's
 * "### V01 — Getting started"), verbatim, or a persona-adjudicated edit from
 * ADJUDICATION.md, because those lines were budgeted at ~2.4 words/sec and the
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
import { act, beat, caption, chapter, PACE, spotlight } from "./overlay";
// The browser, the recorded context and the shared page live in stage.ts:
// importing it is what registers this file's beforeAll/afterAll, and each act
// reads the page out of stage() rather than closing over a module-level `let`.
import { stage } from "./stage";
import { advance } from "./funnel";

test.skip(!process.env.WARDYN_DEMO, "demo recording — run via `make record-demo` (exports WARDYN_DEMO=1)");

// Sandboxes are real containers — the connectivity probe launches one. This is
// minutes, not seconds. It is a ceiling for waiting on the PRODUCT; the pacing
// the viewer sees comes from overlay.ts.
const SANDBOX_UP = 180_000;

test.describe.configure({ mode: "serial" });

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

  // The "how it works" strip under the hero (intro.tsx's HowItWorksStrip) is
  // what the per-line rings below point at — five cards: "Own identity",
  // "Behind a barrier", "Keys stay brokered by default", "You gate the risky
  // bits", "Everything recorded".
  const howItWorks = page.getByRole("list", { name: "How Wardyn protects each run" });

  // The opening card and the three lines under it are the only chance this
  // series gets to say WHAT WARDYN IS. The first cut said "One command to a
  // governed host", which is a slogan: it describes no mechanism, names no
  // problem, and the hero already on screen behind it makes the same promise
  // better. Both halves now state the actual shape of the product — sandbox,
  // proxy, no resident credential — because a viewer who does not know what
  // this is by second twenty has no reason to watch the other nine videos.
  await chapter(page, "Wardyn", "An agent gets its own sandbox, a policy it cannot exceed, and no credentials to steal");
  // [a] the cold open ran ~20-29s with no motion (ADJUDICATION V01) — the ring
  // now moves per line instead of parking still for all five captions.
  await spotlight(page, howItWorks.getByText("Behind a barrier", { exact: true }).locator(".."));
  await caption(page, "Wardyn runs coding agents in a sandbox with no route out except a proxy you control.");
  await beat(page, PACE.read);
  // [a] "proxy" landed as a known noun from 0:09 with no gloss (ADJUDICATION
  // V01) — one plain-language clause, at first use.
  await caption(page, "The proxy is a checkpoint outside the box — every request must pass through it.");
  await beat(page, PACE.read);
  await spotlight(page, howItWorks.getByText("Keys stay brokered by default", { exact: true }).locator(".."));
  await caption(page, "It never hands them a key — the proxy attaches credentials on the way out.");
  await beat(page, PACE.read);
  await spotlight(page, howItWorks.getByText("Everything recorded", { exact: true }).locator(".."));
  await caption(page, "So a compromised agent has nothing to steal, and every attempt is on the record.");
  await beat(page, PACE.read);
  await spotlight(page, null);
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

  // [a] "Get started" used to be an uncaptioned click, reported as a jump-cut
  // (ADJUDICATION V01).
  await act(
    page,
    page.getByRole("button", { name: /^Get started/ }),
    "Get started opens the setup funnel — ten steps, only three of them essential.",
  );
  // "Step 1 of 10" is the script's own SCREEN note and Track B froze the funnel
  // STRUCTURE (DA12) before this spec was extracted. A different count means the
  // funnel gained or lost a step — and the demos (now 05a-what-it-stops.spec.ts)
  // still occupy five of them, so the video would be wrong in a way no caption
  // could cover.
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
  // [a] per-column spotlight during the walk (ADJUDICATION V01) — the ring
  // moves per tier instead of parking on the whole group for six captions.
  await spotlight(page, tiers.getByRole("radio", { name: /Fence/ }));
  await caption(page, "Fence is a container — ordinary Linux isolation, sharing your machine's kernel.");
  await beat(page, PACE.read);
  await caption(page, "Fast to start, and the weakest: a kernel bug is a way out of it.");
  await beat(page, PACE.read);
  await spotlight(page, tiers.getByRole("radio", { name: /Wall/ }));
  await caption(page, "Wall runs gVisor, which hands the agent a kernel written in software.");
  await beat(page, PACE.read);
  await caption(page, "Its system calls hit that copy, so an exploit has nothing real to land on.");
  await beat(page, PACE.read);
  await spotlight(page, tiers.getByRole("radio", { name: /Vault/ }));
  await caption(page, "Vault is a micro virtual machine, through Kata — its own kernel, its own hardware.");
  await beat(page, PACE.read);
  await caption(page, "Strongest, slowest to boot, and it needs virtualization the host may not have.");
  await beat(page, PACE.read + 600);

  // The permanent "Doesn't stop:" row (copy.ts's RESIDUAL_PREFIX) is the whole
  // honesty claim the next line makes — a tier matrix that also prints what each
  // barrier does NOT protect against.
  // [a] the ring used to land on the "Doesn't stop:" label, not the row
  // (ADJUDICATION V01) — environment-step.tsx renders it as a real <tr>
  // (RowLabelled), so walking up to the nearest one catches the per-tier cells.
  await spotlight(page, page.getByText("Doesn't stop:", { exact: true }).locator("xpath=ancestor::tr[1]"));
  await caption(page, "Every tier also prints what it does not stop. No barrier is a promise of safety.");
  await beat(page, PACE.read);
  // [a] "only offers the ones it can enforce" had no receipt on this all-Ready
  // host (ADJUDICATION V01) — reworded to what this screen can actually show.
  await caption(page, "Wardyn probed this host first — a tier it can't enforce shows up disabled. Here, all three passed.");
  await beat(page, PACE.read + 600);
  await spotlight(page, null);

  // Click the READY column. On the demo host (WSL2 + Docker Desktop) that is
  // Fence and only Fence — the same fact V06's script records for the same
  // stack. A radio for a tier this host cannot build renders DISABLED, so if
  // that ever stops being true this click fails loudly instead of filming a
  // selection that did not happen.
  const fence = tiers.getByRole("radio", { name: /Fence/ });
  await expect(fence).toBeEnabled();
  // [a] the pick itself used to be filmed mute (ADJUDICATION V01).
  await act(page, fence, "Fence is enough for today's demos — one click, saved as the default for new runs.");
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
  // [a] the corporate-proxy line used to leave the ring on the sidebar chip
  // (ADJUDICATION V01) — point at the field "here" actually names.
  await spotlight(page, page.getByLabel(/Proxy URL/i));
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
  // [a] the egress-redirection tab used to be narrated before it existed on
  // screen, then sit silent for ~11s once it appeared (ADJUDICATION V01,
  // series ruling S5) — click the tab itself (the same state change the first
  // "Next" press used to trigger blind) and speak over what it reveals.
  await act(
    page,
    page.getByRole("tab", { name: /Egress redirection/ }),
    "The other tab redirects those — npm, PyPI, container images — at your internal mirrors.",
  );
  await caption(page, "The mirror's token is named here too — injected proxy-side; the sandbox never holds it.");
  await beat(page, PACE.read);

  // Next: the tab click above already did what the first "Next" press used to
  // do on its own (setup-screen.tsx's corp_network onNext override swaps the
  // tab instead of leaving the step while Host proxy is showing), so this now
  // advances straight to Secrets. advance() still absorbs a second Next if the
  // gate ever needs one — "press Next until the step counter changes", not
  // "press Next once".
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
  // [a] "captured once… and stored by Wardyn" used to be where the model-
  // credential story stopped (ADJUDICATION V01).
  await caption(page, "It lives in Wardyn's own store on this host — runs borrow it, they never hold it.");
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
// Conclusion — the video ends here, on the funnel's three setup steps, by the
// 12-episode restructure (2026-08-20, owner-approved). The five guardrail
// demos that used to be act 3 are their own episode now — "What it stops"
// (05a-what-it-stops.spec.ts) — so getting-started closes the moment ITS OWN
// subject — a host with a barrier, a proven network path, and its secrets —
// is proven, and hands off to what comes next. Workspace onboarding (video
// 02) and running one (video 03) were already someone else's subject before
// this split; the demos joining them is what's new.
// ---------------------------------------------------------------------------

test("V01 act 3 — conclusion", async () => {
  test.setTimeout(120_000);
  const page = stage();

  await chapter(page, "What you just saw", "A governed host, ready for real work");
  await caption(page, "That is Wardyn: your agents and workloads run in sandboxes, under policies you write.");
  await beat(page, PACE.read);
  await caption(page, "In one sitting this host got a barrier, a proven network path, and its secrets.");
  await beat(page, PACE.read);
  // [a] nobody could answer "what did setup require" (ADJUDICATION V01) — the
  // one quiz question every persona failed.
  await caption(page, "From a fresh install that was: one barrier click, one connectivity test, one sign-in.");
  await beat(page, PACE.read + 600);
  await caption(page, "Next: give a run something real to work on — a workspace.");
  await beat(page, PACE.read);
  await caption(page, "Run anything. Keep your keys.");
  await beat(page, PACE.chapter);
  await caption(page, "");
  await silentCard(page, "Next — 02: Add a workspace");
});
