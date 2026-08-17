/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * Video 01 of the 0.5 series — "Getting started".
 *
 * WHAT THIS FILMS. One command has already brought a control plane up on this
 * machine; nothing has been configured and nothing has run. The take opens on
 * the first-boot hero, walks the whole Getting Started funnel — the barrier, the
 * mandatory connectivity gate, the model step, the five hands-on guardrail
 * demos, one real workspace — and STOPS on the click of "Finish setup". No run
 * is launched here. Launching a run is video 02, and this driver ending one
 * click early is the seam between them.
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
 *      The cold open's "no account, no cloud sign-in" has to film true.
 *
 * Selectors are getByRole + accessible names, matching ui/e2e/fixtures.ts and
 * the rest of the suite: a copy change breaks this loudly and in one place,
 * markup churn does not break it at all.
 */

import { test, expect, type Page } from "@playwright/test";
import { FUNNEL_DEMOS, WORKSPACE_NAME, WORKSPACE_PATH } from "./task";
import { act, beat, caption, chapter, PACE, spotlight, typeInTerminal } from "./overlay";
// The browser, the recorded context and the shared page live in stage.ts:
// importing it is what registers this file's beforeAll/afterAll, and each act
// reads the page out of stage() rather than closing over a module-level `let`.
import { stage } from "./stage";
import { advance, APPROVAL_APPEARS, clearWorkspace, decide } from "./funnel";

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
  property: string;
};

const DEMO_PROPERTY: Record<string, string> = {
  "sealed-box": "Deny is the default, and a refusal needs no human.",
  "fail-then-approve": "A refusal can also ask — the boundary moves when a person moves it.",
  "held-at-the-door": "The decision can happen while the request is still open.",
  "lines-that-cant-be-crossed": "Some limits are not policy at all. No setting can open them.",
  "once-or-for-good": "An approval has a scope, and the narrowest is one connection.",
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

  await chapter(page, "Wardyn", "One command to a governed host");
  await caption(page, "Wardyn runs coding agents, and any workload, inside a sandbox you govern.");
  await beat(page, PACE.read);
  await caption(page, "This series is for whoever has to sign off on that.");
  await beat(page, PACE.read);
  await caption(page, "Video one: one command, and this host is governed and ready to run.");
  await beat(page, PACE.read);

  // B1 — the hero, then the LIVE host chips under it. The chips are the honest
  // half of the cold open: they are read off this machine's real SetupStatus,
  // not a marketing screenshot, which is exactly what the SAY line claims.
  await spotlight(page, hero);
  await beat(page, PACE.read);
  await spotlight(page, page.getByText("This host right now:").locator(".."));
  await caption(page, "No account, no cloud sign-in — this control plane is local, and already up.");
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
  await expect(page.getByRole("heading", { name: "Pick your barrier", level: 2 })).toBeVisible({ timeout: 30_000 });
  await caption(page, "A barrier is the wall between the agent and your machine — Wardyn builds three.");
  await beat(page, PACE.read + 600);

  const tiers = page.getByRole("radiogroup", { name: "Barrier tier" });
  await spotlight(page, tiers);
  await caption(page, "Fence shares your kernel; Wall replaces it in software; Vault is its own machine.");
  await beat(page, PACE.read + 900);

  // The permanent "Doesn't stop:" row (copy.ts's RESIDUAL_PREFIX) is the whole
  // honesty claim the next line makes — a tier matrix that also prints what each
  // barrier does NOT protect against.
  await spotlight(page, page.getByText("Doesn't stop:", { exact: true }));
  await caption(page, "It probes this host and tells you honestly which of the three it can enforce.");
  await beat(page, PACE.read + 900);
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

  // --- B3 — Corporate network, the one gate --------------------------------
  await expect(page.getByRole("heading", { name: "Corporate network", level: 2 })).toBeVisible({ timeout: 30_000 });
  await caption(page, "This is the only step that can refuse to continue. It wants proof.");
  await beat(page, PACE.read + 600);

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
    "It launches a throwaway sandbox down the same path a real run takes.",
  );

  // A REAL sandbox goes out and comes back. .first(): the verdict chip renders
  // in the step body AND as the rail's step badge, and they say the same thing.
  const reached = page.getByText(/^Reached · direct/).first();
  await expect(reached).toBeVisible({ timeout: SANDBOX_UP });
  await spotlight(page, reached);
  await caption(page, "Reached, direct. A corporate proxy or an internal mirror would be configured right here.");
  await beat(page, PACE.read + 900);
  await spotlight(page, null);

  // Next ×2: the first press answers by revealing the step's other tab rather
  // than moving on, which is exactly what advance() is built to absorb.
  await advance();

  // --- B4 — Connect your model --------------------------------------------
  await expect(page.getByRole("heading", { name: "Connect your model", level: 2 })).toBeVisible({ timeout: 30_000 });
  await caption(page, "This model was connected at the command line before recording. The step is optional.");
  await beat(page, PACE.read + 600);

  // PRECONDITION, ASSERTED. The line above says the model is already connected.
  // If the off-camera token-stdin connect did not land, the lane reads
  // unconnected and the narration is a claim the screen refuses — the exact
  // green-take-over-a-failed-screen failure this series has shipped before.
  const subLane = page.getByRole("radio", { name: /Claude subscription/ });
  await expect(subLane).toContainText("Connected", { timeout: 30_000 });
  await spotlight(page, subLane);
  await beat(page, PACE.read);

  // The card footer. Its real text is longer than the script's shorthand — it
  // carries the Bedrock-SSO exception — so point at the sentence that exists.
  await spotlight(page, page.getByText(/keys never enter the sandbox/i).first());
  await caption(page, "The token never enters the sandbox — the proxy injects it on the wire instead.");
  await beat(page, PACE.read + 900);
  await spotlight(page, null);

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
  // B5 is a card, and it is spoken: "The guardrails. Five sandboxes, five ways
  // the boundary holds."
  await chapter(page, "The guardrails", "Five sandboxes, five ways the boundary holds");

  // See the audit-panel block at the bottom of the loop: the trail line is a
  // fact about Wardyn, said once, not a per-demo refrain.
  let auditLineSpoken = false;

  for (const demo of V01_DEMOS) {
    await expect(page.getByRole("heading", { name: demo.label, level: 2 })).toBeVisible({ timeout: 60_000 });
    // Deliberately NOT spoken: the demo's title and blurb are already on screen,
    // in a heading the camera is looking straight at, and the script budgets
    // "one line spoken over each demo" — that line is demo.property below, the
    // one thing the screen does NOT say. Reading the heading aloud cost ~89
    // words across the five demos and pushed a 6:30 video to 7:15.
    await beat(page, PACE.read + 700);

    // No caption on the click either. "Starting a throwaway sandbox under
    // exactly that policy" is pure step-narration — it describes the button
    // being pressed, five times, while the sandbox visibly starts.
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
// B11-B12 — a workspace, review, and the last click of the video
// ---------------------------------------------------------------------------

test("V01 act 4 — onboard a workspace, finish setup", async () => {
  test.setTimeout(300_000);
  const page = stage();

  await expect(page.getByRole("heading", { name: "Onboard a workspace", level: 2 })).toBeVisible({ timeout: 60_000 });

  // Onboarding the SAME name twice is rejected and silently leaves the dialog
  // open, so a second run of the driver against a stack that already has this
  // workspace would fail here. Clear it first, off camera, so the act always
  // films an actual creation. (A full `make record-demo` resets the stack and
  // never needs this; iterating with --no-reset does.)
  await clearWorkspace();
  await caption(page, "A workspace is a directory a run may attach — nothing else gets mounted.");
  await beat(page, PACE.read + 600);

  // The step's trigger is state-dependent (step-bodies.tsx): an empty install
  // says "Onboard your first workspace", and only once one exists does it
  // become "Add workspace". The recording always runs against an empty one, but
  // matching both keeps an --no-reset iteration working.
  await act(page, page.getByRole("button", { name: /Onboard your first workspace|Add workspace/ }).first());
  const dlg = page.getByRole("dialog");
  // By ROLE, not text: "Add workspace" is both the dialog title and its submit
  // button, so a bare getByText is a strict-mode violation.
  await expect(dlg.getByRole("heading", { name: "Add workspace" })).toBeVisible();

  // Source/image cards are OptionCards — aria-pressed buttons, not radios
  // (form-primitives.tsx). Their accessible name carries the hint text too, so
  // match on a prefix rather than exactly.
  await act(page, dlg.getByRole("button", { name: /Local directory/ }));
  await spotlight(page, dlg.getByLabel("Path on this host"));
  await dlg.getByLabel("Path on this host").fill(WORKSPACE_PATH);
  await dlg.getByLabel("Name", { exact: true }).fill(WORKSPACE_NAME);
  await spotlight(page, null);
  await beat(page, PACE.read);

  const advanced = dlg.getByText("Advanced", { exact: true });
  if (await advanced.isVisible().catch(() => false)) {
    await act(page, advanced, "Under Advanced: where it lands inside the box, and writes, which are off by default.");
    await spotlight(page, dlg.getByLabel("Mount path"));
    await beat(page, PACE.read + 600);
    await spotlight(page, null);
  }

  // LOAD-BEARING FOR VIDEO 02, NOT FOR THIS ONE: `writable` defaults to FALSE
  // (add-workspace-dialog.tsx), so without this tick the directory mounts
  // READ-ONLY and V02's agent edits never reach the host — that video runs,
  // reports success, and its "Files changed" finale shows an empty diff. V01
  // ends before any run, so nothing here would catch it. Granting write access
  // on camera is the honest beat anyway: a run that edits your code should have
  // to be given permission to.
  const writable = dlg.getByRole("checkbox", { name: /Allow writes/ });
  await expect(writable).toBeVisible();
  await act(page, writable);
  await expect(writable).toBeChecked();
  await beat(page, PACE.read);

  await act(page, dlg.getByRole("button", { name: "Add workspace" }));
  await expect(dlg).toBeHidden({ timeout: 60_000 });
  await beat(page, PACE.read);

  await advance();

  // --- B12 — Review readiness ---------------------------------------------
  await expect(page.getByRole("heading", { name: "Review readiness", level: 2 })).toBeVisible({ timeout: 60_000 });
  await caption(page, "Review checks this host, not a checklist: blocking, worth a look, ready.");
  await beat(page, PACE.read + 900);

  // "Nothing blocking" is a claim about what is on screen, so prove it. The
  // Blocking section renders only when a check actually FAILED
  // (step-bodies.tsx's `group`), so its absence is the assertion — and a real
  // blocker on shoot day would otherwise be narrated straight over.
  await expect(page.getByText("Blocking", { exact: true })).toHaveCount(0);
  await caption(page, "Nothing blocking. Finish setup — a governed host, ready for real work.");
  await beat(page, PACE.read + 600);

  await act(page, page.getByRole("button", { name: "Finish setup" }));

  // The funnel is genuinely behind us — the step counter is the honest witness
  // (the Runs nav link is in the shell on every screen, including the funnel).
  await expect(page.getByRole("link", { name: /^Runs/ })).toBeVisible({ timeout: 60_000 });
  // Generous: "Finish setup" writes before it navigates, and the default 5 s
  // expect budget would fail the LAST click of a seven-minute take over a slow
  // POST rather than over anything being wrong.
  await expect(page.getByText(/^Step \d+ of \d+$/)).toHaveCount(0, { timeout: 30_000 });
  await beat(page, PACE.read);

  // RULED 2026-08-17, and the reviewer was right to escalate rather than reword
  // a verbatim SAY line on its own authority. The script's B12 direction said
  // "Finish setup → empty Runs board". THE BOARD IS NOT EMPTY: act 3's five
  // demos each create a real agent_run (demo-screen.tsx's api.createRun) and
  // B3's connectivity probe creates a sixth (site_config_probe.go's
  // newStepRun). None is titled and none carries a hidden flag, so runs.tsx
  // renders all six as a loose card grid. The original lines — "nothing has
  // run", "nothing has run yet" — would have played over them and been simply
  // false on camera, in the one video most viewers watch first.
  //
  // Hiding them was the other option and it was rejected: a governance product
  // that quietly drops its own runs off the board is lying in the exact place
  // it asks to be trusted. So the line NAMES what is on screen instead. It
  // costs one word, it is true, and it lands a better point than the empty
  // board would have — setup's own probes are governed and audited like
  // anything else, which is the whole promise this video is selling.
  //
  // The last line is a deliberate diegetic bookend, not the series motif
  // (SV6): this video filmed that exact string as the hero in its cold open.
  await expect(page.getByRole("link", { name: /^Runs/ })).toBeVisible();
  await caption(page, "Those cards are setup's own sandboxes — your first run is video two.");
  await beat(page, PACE.read + 900);
  await caption(page, "Run anything. Keep your keys.");
  await beat(page, PACE.chapter);
  await caption(page, "");
  await silentCard(page, "Next — 02: Your first run");
});
