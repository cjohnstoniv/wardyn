/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// RETIRING (Workstream C lane 5, mechanical renumber, 2026-08-24). Number
// slot 08 now belongs to the autonomous-agent episode (08-autonomous-agent.
// spec.ts, moved from old 07) — this file was renamed OUT of the numbered
// 01-12 sequence (git history preserved via `git mv`) so the two files could
// stop colliding. Its content — the barrier-tier honesty story and the
// saved-policy-reuse teaching below — RETIRES INTO the new episode 05
// ("Your first policy — the panel, templates, THE METER, save"), per
// Workstream C of /home/cjohn/.claude/plans/merry-snacking-harbor.md. Lane 6
// absorbs this material when it authors 05-*.spec.ts (a NEW file — lane 5
// deliberately did not create it or place a placeholder at slot 05, since the
// new episode's real content, including the safety-meter beats, does not
// exist yet). Delete this file once its material has been folded in.
//
// UNCHANGED BELOW: no narration/content edits — that is lane 6's call to
// make while rewriting this into the new episode.

/*
 * Video 08 of the series — Policies & confinement tiers.
 *
 * WHAT THIS FILMS. Two halves of one idea. First the HONESTY of the barrier
 * picker: Settings' Host card names, per tier, whether this machine can build
 * it — and never claims one it cannot enforce. Then the REUSABLE half of
 * governance: write a policy once, and a later run references it by id instead
 * of re-declaring its whole envelope on the New Run form. The payoff is the
 * cockpit's Identity widget, whose Policy row is the control plane's own answer
 * to "what was this agent actually allowed to do".
 *
 * ── THE ONE THING THAT CHANGED SINCE THE SCRIPT WAS WRITTEN ────────────────
 * The plan's V06 section says this host reads "Fence Ready / Wall Needs setup /
 * Vault Incompatible here" and that a policy floored at Wall "refuses to start
 * here". BOTH ARE FALSE TODAY. /healthz on this stack answers
 *   confinement_classes: ["CC1","CC2","CC3"]
 *   confinement_substrates: {CC1: oci/runc, CC2: oci/runsc, CC3: oci/kata}
 * so all three columns read "Ready" and the top-bar chip is VAULT (not Fence)
 * — no tier is un-buildable here, so B4 can no longer just let the New Run
 * screen's resolveDefaultCc float this run up to the host's strongest class
 * and call that "the floor holding". Instead B4 DRIVES it: rings Fence, which
 * the Barrier Seg has DISABLED beneath the attached policy's Wall floor (and
 * says so in its own reason line), then clicks Wall — the weakest tier the
 * floor still allows — so the run that actually launches is pinned at Wall,
 * not wherever this host's default happens to sit.
 *
 * So nothing below names a tier or a status the host might contradict, with
 * one deliberate exception (next paragraph):
 *   - B1 reads the host's real answer in beforeAll and asserts exactly that
 *     many "Ready" chips (and exactly that many not-ready ones). It survives a
 *     stack where Wall gets uninstalled or Vault loses /dev/kvm, in either
 *     direction, and the narration teaches the VOCABULARY (ready / needs setup
 *     / incompatible) rather than asserting a verdict out loud.
 *   - B2's floor line no longer says "set it to Wall"; it states the rule
 *     (a floor above what a host can build refuses to start there), which is
 *     true on every host.
 *   - B4 reads the launched run back off the wire and asserts the header chip
 *     shows the class the CONTROL PLANE enforced, named by /healthz's own
 *     confinement_names map — that ASSERTION never hardcodes a tier, even
 *     though the floor beat right before it does say "Fence" and "Wall" out
 *     loud (see below).
 * The two labels this file DOES hardcode are "Fence" and "Wall": the floor of
 * the policy IT AUTHORS (min_confinement_class: CC2), and the weaker tier B4
 * deliberately rings against it. Both are our own spec and our own choice of
 * click — never a claim about what the host can build. (beforeAll does assert
 * this host can BUILD CC2, because B4 clicks Wall for real; without it a
 * CC2-less host would park act() on a disabled button for 45s.)
 *
 * ── WHAT THE POLICY SURFACE CHANGED HERE ───────────────────────────────────
 * This episode's premise — "two surfaces authoring one object" — is now the
 * product's own answer: /policies' editor body and /runs/new's Policy card are
 * the SAME component (wardyn/policy-panel.tsx). So B2 films what that bought:
 * the template chips the editor now opens with, and the helper rail that
 * documents every key. B3/B4's saved-policy path is unchanged in intent, and
 * changed in exactly two mechanics: "Saved policy" is a mode-row OptionCard
 * (an aria-pressed <button>, not a radio), and the barrier FLOOR is now
 * enforced by DISABLING every tier below it on the Seg — a standing refusal
 * with its own reason line, where the old screen detached the policy the
 * moment you touched the tier. That is B4's floor beat, and it is stronger
 * footage than either branch the old file had to hedge between.
 *
 * KEYLESS, and by choice rather than by the plan (whose STAGING line says
 * "model pre-connected (no rail warning)"). B3 picks "Shell command", so this
 * take needs no agent, no model, no quota: the Credentials rail's warning is
 * gated on `isAgent && llmReady === false` (new-run-screen.tsx:889-893), so a
 * command run cannot raise it whatever the stack's model posture is. Every beat
 * survives: the rail's Policy section and the Identity widget's Policy row both
 * key off the POLICY, never the agent.
 *
 * OPERATOR-GATED: /policies' "New policy" button is disabled for a non-operator
 * (policies.tsx:141). No sign-in beat is needed because the series shoots in
 * LOCAL MODE, where every session is `local:operator` — but B2 asserts the
 * button is ENABLED before clicking it, so a take accidentally shot against an
 * SSO stack fails loudly here instead of clicking a dead control for 45s.
 *
 * FRICTION NOTES, RE-VERIFIED AGAINST SOURCE (2026-08-23, after the shared
 * policy panel landed). Three of the plan's four Track-A findings for this
 * video are FIXED on this branch:
 *   (1) FIXED — the launch payload is gated on the MODE ROW
 *       (`useSaved && state.selectedPolicyId`, new-run-screen.tsx's
 *       buildRunInput), not a stale selectedPolicyId alone. What detaches has
 *       narrowed to exactly one trigger — EDITING THE SPEC TEXT — because the
 *       tier that used to detach the policy is now simply disabled instead
 *       (see B4).
 *   (2) FIXED — the rail HAS a Policy section: it names the stored policy, its
 *       barrier floor and its host count, and says the page's own edits are
 *       not merged into it ("It launches by reference, so nothing on this page
 *       is merged into it."). The plan's B3 camera direction — "camera stays
 *       on the select, rail does NOT reflect saved policies today" — is
 *       therefore OBSOLETE, and B3 below spotlights the rail instead.
 *   (3) STILL LIVE — the editor's STARTER_SPEC floors at CC2 (it IS the
 *       panel's Minimal template now, policies.tsx's one-const re-export). B2
 *       never edits it in place: it clicks a TEMPLATE CHIP and then PASTES the
 *       prepared spec over the whole textarea with .fill(), which replaces the
 *       value in one call exactly like a real clipboard paste. The chip is
 *       deliberately NOT Minimal — that is what the dialog already prefills,
 *       so clicking it would be a dead click on camera.
 *   (4) FIXED — the screen re-resolves the persisted default against the
 *       host's REAL class list, so Settings' "saved as your default barrier"
 *       promise holds here too. Picking a policy only ever UP-clamps that
 *       default, never lowers it, which is why B4 clicks Wall for real
 *       instead of expecting the pick to have settled the run there.
 *
 * PACING. One stretch of this video is nothing but waiting on a container: B4's
 * launch (POST /runs dispatches SYNCHRONOUSLY, so the navigate does not happen
 * until the sandbox is up) plus the seconds the command itself takes. It is
 * wrapped in ffwdStart/ffwdEnd and squeezed by scripts/demo-ffwd.py afterwards.
 * NOTHING SPEAKS inside a span — a line spoken over frames the encoder throws
 * away lands 12x early and drags every later cue with it — and the ffwdStart is
 * preceded by a beat(200) that DRAINS the previous line, because caption() only
 * SCHEDULES speech. ffwdEnd fires in a `finally`, so a failed take still closes
 * its span. Nothing else is spanned: every other wait here is a page render.
 *
 * WHY B4 WAITS FOR THE RUN TO FINISH. Not thoroughness — geometry. The cockpit
 * canvas picks its preset from the run's own state (canvas.tsx:110-113: live
 * while running, finished once terminal) and the two presets put the Identity
 * widget in different places (widget-registry.ts:155-162). A short `echo` run
 * goes terminal within seconds of landing, so narrating over the live preset
 * means the ring is parked on a tile that is about to move. Waiting first also
 * makes the take's own claim ("every run after it, already governed") a
 * finished fact rather than a promise, and gives verify-demo-take.sh a terminal
 * state to check.
 *
 * WHAT CAN STOP EXISTING MID-WAIT — see WAIT_UNLESS_GONE below. Every wait whose
 * surface has a mutually-exclusive twin is raced against it, so a bad take fails
 * in seconds with a NAME instead of polling for minutes against a node that can
 * no longer appear.
 *
 * This is NOT a test. It asserts only enough to keep itself honest and to know
 * when to advance; a failure here means the recording is wrong, not that the
 * product is broken. It runs against the REAL compose stack on :8080.
 *
 * Driven by `scripts/record-demo.sh --video 08`, which globs this exact filename
 * and names the take wardyn-08-policies-and-confinement-<stamp>.mp4 — so this
 * FILENAME IS LOAD-BEARING. It self-skips without WARDYN_DEMO=1 so a bare
 * `pnpm e2e` can never point a browser at a developer's live stack and start
 * deleting policies.
 *
 * The demo project records HEADLESS (playwright.config.ts): the browser records
 * ITSELF, there is no OS window during a take, and nothing here may depend on
 * window geometry. Nothing does — every locator is a role, a label or page text.
 *
 * Runs after V01's reset, on a shared already-set-up local-mode stack. It makes
 * no assumption about which page "/" lands on: it waits for the app shell (the
 * top bar's own New-run button) rather than a specific route.
 *
 * Every literal below exists in ui/src today (re-read 2026-08-18 against
 * app-shell.tsx, settings-screen.tsx, environment-step.tsx, copy.ts,
 * policies.tsx, new-run-screen.tsx, run-detail-summary-header.tsx and
 * run-detail/widgets/identity.tsx).
 */

import { test, expect, type Locator, type Page } from "@playwright/test";
import { act, beat, caption, centerInFrame, chapter, ffwdEnd, ffwdStart, PACE, spotlight } from "./overlay";
// stage.ts is the rig: importing it registers this file's beforeAll/afterAll
// (one browser, one context, one recorded page), and every beat reads the page
// out of stage() inside a test body rather than closing over a module binding.
import { stage } from "./stage";
// S6 stale-stack hygiene — see the beforeAll below.
import { sweepStaleState } from "./sweep";

test.skip(!process.env.WARDYN_DEMO, "demo recording — run via `make record-demo` (exports WARDYN_DEMO=1)");

test.describe.configure({ mode: "serial" });

// ---------------------------------------------------------------------------
// This video's nouns. Deliberately LOCAL, not in task.ts: nothing else in the
// series creates a policy, and a shared constant is how two videos end up
// fighting over one control-plane row.
// ---------------------------------------------------------------------------

/** The policy this video authors and then launches under. Deleted per take. */
const POLICY_NAME = "nightly-triage";

/**
 * The prepared spec B2 PASTES. Four fields — and min_confinement_class is CC2
 * ON PURPOSE: it is the floor B4 films the Barrier Seg refusing to go below.
 * (An older comment here claimed a SAY line "The spec is four fields."; no
 * such line exists in the owner's script — episodes-03-12-scripts-current.md's
 * Episode 08 §B2 — so nothing spoken depends on the field count. The count
 * still matters to the VERIFIER, which asserts these exact four values:
 * scripts/verify-demo-take.sh's V08_POL_* checks.)
 *
 * The paste-proof leans on the egress-domain count: B2 clicks the "Package
 * registries" template chip first (eleven hosts), so a paste that silently
 * failed leaves eleven on the row instead of two. The floor chip alone cannot
 * tell the two specs apart — that template floors at CC2 as well.
 */
const POLICY_SPEC = {
  allowed_domains: ["github.com", "npmjs.org"],
  first_use_approval: "wait_for_review",
  min_confinement_class: "CC2",
  eligible_grants: [],
} as const;

/**
 * How the SAVED policy's own fields must render — OUR spec, so these are
 * host-independent and safe to hardcode (unlike anything about the host's
 * barriers). "Wall" is CC_META.CC2.label (cc-meta.ts:58); "2 domains allowed"
 * is egressSummary()'s exact phrasing (policies.tsx:87-91).
 */
const POLICY_FLOOR_LABEL = "Wall";
const POLICY_EGRESS_SUMMARY = "2 domains allowed";

/** The owner's staccato lines read fast; PACE.read after one is dead air. */
const BEAT_SHORT = 1400;

/**
 * B3's run. A Shell command, not an Agent task — see the file header's KEYLESS
 * note. The echo can't fail; the curl is the allowlist's own trial (Sam: "an
 * echo doesn't test an allowlist") — npmjs.org is one of POLICY_SPEC's own
 * allowed_domains, so the finished run's ledger carries a real allow beside
 * the implicit wardynd one every run gets. Template literal, not a plain
 * string: the -w format string needs the shell's own single quotes, and its
 * \n must stay the literal two characters backslash-n (written `\\n` here) —
 * an unescaped `\n` in a JS template literal is a real newline, not text.
 */
const DEMO_RUN_TITLE = "Nightly triage, governed by policy";
const DEMO_COMMAND = `echo "nightly triage: running under the saved policy" && curl -sS -o /dev/null -w 'npmjs.org: %{http_code}\\n' https://npmjs.org`;

// Real containers, so this is minutes. A ceiling for waiting on the PRODUCT —
// the pacing the viewer sees comes from overlay.ts, never from here.
const SANDBOX_UP = 180_000;

/** RunStateBadge's labels (runStateMeta, primitives.tsx:143-156). TITLE CASE. */
const RUN_DONE = "Completed";
const RUN_BROKE = /^(Failed|Stopped|Killed)$/;

// ---------------------------------------------------------------------------
// Host truth, read once in beforeAll and asserted against on camera.
//
// The whole B1 beat is a claim about THIS machine, and the plan's own three-way
// verdict is already stale (file header). So the host answers for itself: the
// classes it can build, and the display name it gives each one. /healthz is the
// same probe the New Run wizard uses (new-run-screen.tsx:262-286), so the
// wizard's barrier choice and this file's expectation can never disagree.
// ---------------------------------------------------------------------------

/** e.g. ["CC1","CC2","CC3"] — every barrier this host can actually build. */
let hostClasses: string[] = [];
/** e.g. {CC1:"Fence", CC2:"Wall", CC3:"Vault"} — the server's own names. */
let hostNames: Record<string, string> = {};

/** Filled in by B2, read by B3 and B4. */
let createdPolicyId = "";

function apiHeaders(): Record<string, string> | undefined {
  // Local mode (the series' posture) has no auth at all; the token lane exists
  // for a stack booted with one. Same shape 07's restageWorkspace uses.
  return process.env.WARDYN_DEMO_TOKEN
    ? { Authorization: `Bearer ${process.env.WARDYN_DEMO_TOKEN}` }
    : undefined;
}

// ---------------------------------------------------------------------------
// WAIT_UNLESS_GONE — the one shape every racy wait in this video is written in.
//
// Copied from 07 (file-local there too, deliberately): two mutually-exclusive
// outcomes, raced, so the losing one becomes an immediate NAMED failure instead
// of a wait that polls for minutes against a node that can no longer appear.
// This file's three losers are:
//   - the New Run rail's error line, which appears INSTEAD of a navigation when
//     the control plane refuses the launch (a 422/400 body, rendered verbatim);
//   - a terminal state that is not "Completed" — the run broke rather than ran.
// `goneMeans` is the one addition over 07's copy: 07's losers are all "the run
// ended", ours are not, and a diagnostic that names the wrong cause is worse
// than a generic timeout.
//
// `want` is any Playwright wait already in flight (an `expect(…)` assertion or a
// `locator.waitFor`). The losing promise keeps polling in the background until
// its own timeout; both branches carry a rejection handler, so that is a
// dangling poll and never an unhandled rejection.
// ---------------------------------------------------------------------------
async function waitUnlessGone(
  want: Promise<unknown>,
  gone: Locator,
  timeout: number,
  why: string,
  goneMeans = "the other outcome landed first",
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
      ? `${why} — ${goneMeans}.`
      : `${why} — the wait timed out (${timeout / 1000}s) with nothing else to blame.`,
  ).toBe("ok");
}

// ---------------------------------------------------------------------------
// Staging — the part that must not be prose.
//
// OFF-CAMERA PRECONDITIONS THE OPERATOR STILL OWNS (nothing below can encode
// these; check them before the take rolls):
//   1. The compose stack is up on :8080 and reports at least one confinement
//      class. A host with none renders the top bar's "No barrier" chip instead,
//      whose aria-label differs — B1's very first click would fail.
//   2. The stack has NOT been reset since the earlier videos (record-demo.sh
//      --video 08 already defaults DO_RESET=0; never pass --reset here).
//   3. The session is an OPERATOR. Local mode is, automatically; an SSO stack
//      shot as a member fails at B2's enabled-button assertion.
//   4. No model needed. This video is keyless end to end.
// ---------------------------------------------------------------------------

/**
 * Delete any policy already named POLICY_NAME, sweep stale approvals/runs
 * (S6), and read the host's own barrier truth — all off camera, before the
 * first beat.
 *
 * The delete is per-take hygiene: a stale `nightly-triage` left by a prior
 * --no-reset take makes B2's getByRole("row", …) a strict-mode violation (two
 * matches), which fails confusingly halfway through rather than here. It is
 * also what makes this video immediately re-takeable: the second take is
 * byte-identical to the first.
 *
 * sweepStaleState() takes no workspace filter: this video attaches no
 * workspace of its own, and sweep.ts's own comment sanctions the unscoped
 * call — "the demo stack's runs are all the series' own". It clears the stuck
 * Approvals badge from an earlier take; a stale red "Run failed" card has no
 * delete API (same file's comment) and B1's opening owns naming that one on
 * camera instead.
 *
 * Registered AFTER stage.ts's own beforeAll (import order), so stage() is
 * assigned by the time this runs. Re-guards on WARDYN_DEMO because this hook
 * DELETES a control-plane row: a file-level test.skip must never be the only
 * thing standing between a developer's stack and that.
 */
test.beforeAll(async () => {
  if (!process.env.WARDYN_DEMO) return;
  const page = stage();
  const headers = apiHeaders();

  // S6: deny stale pending approvals and kill any run still active from an
  // earlier take, before the board is ever on camera.
  await sweepStaleState();

  const health = await page.request.get("/healthz", { headers });
  expect(health.ok(), `GET /healthz failed (${health.status()}) — is the stack up on :8080?`).toBe(true);
  const h = await health.json();
  hostClasses = (h.confinement_classes ?? []).filter(Boolean);
  hostNames = h.confinement_names ?? {};
  // Both halves of the video rest on this: B1 films the tier matrix, and B4's
  // run has to actually get a barrier. A host with none renders a different
  // top-bar chip entirely (app-shell.tsx:543).
  expect(hostClasses.length, "this host reports NO confinement classes — there is no barrier to film").toBeGreaterThan(0);
  for (const cc of hostClasses) {
    expect(hostNames[cc], `/healthz names no display label for ${cc}`).toBeTruthy();
  }
  // B4 CLICKS the policy's own floor tier for real (the Seg disables
  // everything below it, so Wall is the weakest tier still on offer). A host
  // that cannot build CC2 would leave that button disabled for a
  // host-capability reason instead, and act() would park on it for the full
  // 45s action timeout — fail here, by name, instead.
  expect(
    hostClasses,
    `this host cannot build CC2 (${POLICY_FLOOR_LABEL}) — the policy this video authors floors there, and B4 clicks it`,
  ).toContain("CC2");

  const res = await page.request.get("/api/v1/policies", { headers });
  expect(res.ok(), `GET /api/v1/policies failed (${res.status()})`).toBe(true);
  const body = await res.json();
  const items: { id?: string; name?: string }[] = Array.isArray(body) ? body : (body?.items ?? []);
  for (const p of items) {
    if (p?.id && p.name === POLICY_NAME) {
      const del = await page.request.delete(`/api/v1/policies/${p.id}`, { headers });
      expect(del.ok(), `could not delete the stale ${POLICY_NAME} policy (${del.status()})`).toBe(true);
    }
  }
});

// ---------------------------------------------------------------------------
// B1 — barrier honesty
// ---------------------------------------------------------------------------

test("B1 — barrier honesty", async () => {
  test.setTimeout(180_000);
  const page = stage();
  await page.goto("/");
  await page.bringToFront();

  // App-shell mounted marker: the top bar's own New-run button is present on
  // every authenticated route, regardless of which page "/" happens to land on
  // for an already-set-up stack (unlike V01, this video does not care).
  await expect(page.locator("header").getByRole("button", { name: "New run" })).toBeVisible({ timeout: 60_000 });

  // Fail here rather than at the end of a silent, caption-less take: every
  // narration call degrades to a no-op by design (overlay.ts's call()), so
  // nothing downstream ever complains about an overlay that failed to install.
  await expect
    .poll(() => page.evaluate(() => typeof (window as unknown as Record<string, unknown>).__demo), {
      timeout: 15_000,
    })
    .toBe("object");

  await chapter(page, "Policies & confinement", "Governance you configure once — and any run can reuse");

  // S6: sweepStaleState() (beforeAll) clears pending approvals and running
  // holdouts, but a red "Run failed" card has no delete API (sweep.ts) — the
  // only true board wipe is V01's reset. Name it once, on camera, rather than
  // let it sit unremarked through these opening captions (~8s, all four
  // personas flagged the silent stretch). Conditional, and outside the owner's
  // dialog track entirely — it only fires against a leftover a prior take's
  // sweep could not clear.
  const staleFailed = page.getByText("Run failed — review what happened").first();
  if (await staleFailed.isVisible().catch(() => false)) {
    await spotlight(page, staleFailed);
    // P6 (dialog review, owner-ratified 2026-08-23): the old line said "take",
    // "video one" and "between videos" — the series' only fourth-wall break.
    // Same job, in-world: the card still has to be explained, but as evidence
    // the board refuses to drop, not as a production note.
    await caption(page, "That red card is a run that failed earlier in this series.");
    await beat(page, BEAT_SHORT);
    await caption(page, "The board keeps it.");
    await beat(page, BEAT_SHORT);
    await caption(page, "Nothing here quietly disappears because it was inconvenient.");
    await beat(page, PACE.read);
    await spotlight(page, null);
  }

  await caption(page, "Up to now, we've been configuring every run by hand.");
  await beat(page, PACE.read);
  await caption(page, "That works.");
  await beat(page, BEAT_SHORT);
  await caption(page, "But it doesn't scale.");
  await beat(page, BEAT_SHORT);
  await caption(page, "A policy lets us take those decisions and make them reusable.");
  await beat(page, PACE.read);

  // The chip's aria-label is the STRONGEST-available form (app-shell.tsx:543);
  // the "No sandbox barrier" variant is impossible after beforeAll's assertion.
  await act(page, page.getByRole("link", { name: "Sandbox barrier — open Settings" }), "Open Settings.");
  await expect(page.getByRole("heading", { name: "Host", level: 3, exact: true })).toBeVisible({ timeout: 30_000 });

  const matrix = page.getByRole("radiogroup", { name: "Barrier tier" });
  await expect(matrix).toBeVisible({ timeout: 30_000 });

  // S3: ring the card, not the heading — the same heading→ancestor fix V01
  // already applies to its own "Doesn't stop:" row.
  await spotlight(
    page,
    page.getByRole("heading", { name: "Host", level: 3, exact: true }).locator("xpath=ancestor::section[1]"),
  );
  // P7 (dialog review, owner-ratified 2026-08-23): episode 02 already walks
  // Fence/Wall/Vault in fifteen lines, including this beat's own "Wardyn checks
  // what this machine can actually support." Eight captions here restated it
  // nearly word for word. Two lines now cover the same two on-screen moments —
  // the Host card (this ring) and the per-tier verdict row (the ring below) —
  // and the second one says the thing 02 could NOT: on this screen the pick is
  // saved into the policy. Beats trimmed with it so the pair still covers the
  // frame; beat() holds for the residual speech, so each long line pays for
  // itself and no silent hole opens between the two rings.
  await caption(page, "You've already met Fence, Wall, and Vault, and the same capability check applies here.");
  await beat(page, PACE.read);
  await spotlight(page, null);

  // THE HONESTY BEAT, said WITHOUT a verdict. The plan's line here named a
  // specific three-way answer ("Fence, ready. Wall, needs setup. Vault,
  // incompatible here") that this host contradicts today — see the file header.
  // ASSERT THE PAYOFF, dynamically. Every available class shows StatusChip
  // "Ready" (copy.ts:56); every unavailable one shows "Needs setup" or
  // "Incompatible here" (copy.ts:57,59) — tierState() has no fourth outcome
  // (environment-step.tsx:224-228). A drift in EITHER direction (Wall gets
  // uninstalled, Vault gains /dev/kvm) keeps this green, while a card that
  // stopped reporting per-tier state at all fails loudly here.
  const readyChips = matrix.getByText("Ready", { exact: true });
  const notReadyChips = matrix
    .getByText("Needs setup", { exact: true })
    .or(matrix.getByText("Incompatible here", { exact: true }));
  await expect(
    readyChips,
    `the Host card does not mark ${hostClasses.length} tier(s) Ready, but /healthz reports ${hostClasses.join("+")}`,
  ).toHaveCount(hostClasses.length);
  await expect(notReadyChips).toHaveCount(3 - hostClasses.length);
  // The header row IS the three columns' state blocks (radio + status chip),
  // so one ring covers whatever verdict this host happens to give.
  await spotlight(page, matrix.getByRole("row").first());
  await caption(
    page,
    "The difference is that this time the choice gets saved with the policy, instead of being made again every run.",
  );
  await beat(page, PACE.read + 400);
  await spotlight(page, null);
});

// ---------------------------------------------------------------------------
// B2 — create a policy
// ---------------------------------------------------------------------------

test("B2 — create a policy", async () => {
  test.setTimeout(180_000);
  const page = stage();

  await act(page, page.getByRole("link", { name: "Policies" }), "Open Policies.");
  await expect(page.getByRole("heading", { name: "Policies", level: 1 })).toBeVisible({ timeout: 30_000 });

  // The operator gate, named. A member's button is disabled (policies.tsx:141)
  // and act() would simply park on it for the full 45s action timeout, which on
  // camera is indistinguishable from the app hanging.
  const newPolicyBtn = page.getByRole("button", { name: "New policy" });
  await expect(
    newPolicyBtn,
    "the New policy button is disabled — this session is not an operator (shoot in local mode)",
  ).toBeEnabled({ timeout: 30_000 });

  await act(page, newPolicyBtn, "New policy.");
  const dlg = page.getByRole("dialog");
  await expect(dlg.getByRole("heading", { name: "New policy" })).toBeVisible();

  await caption(page, "A policy is that same kind of spec, saved once with a name so runs can reuse it.");
  await beat(page, PACE.read);
  // Sam: "the modal's own body text is the most interesting thing on screen."
  // VERIFY the inline-floor clause against the server's confinement_floor
  // behavior (composer/clamp.go's EffectiveConfinementFloor and risk.go's
  // RequiredConfinementFloor both gate an inline spec exactly like a saved
  // one — runs_create.go:437-450 — but re-check before speaking it as fact).
  await spotlight(page, dlg.getByText(/admin-gated config/));
  await caption(
    page,
    "Wardyn checks it before it stores it — a policy that couldn't run doesn't get to sit there looking like it would.",
  );
  await beat(page, PACE.read);
  await spotlight(page, null);

  const nameBox = dlg.getByLabel("Name");
  await spotlight(page, nameBox);
  await nameBox.fill(POLICY_NAME);
  await spotlight(page, null);

  // ── THE TEMPLATE GALLERY ──────────────────────────────────────────────
  // New with the shared panel, and the reason this episode's premise is now
  // the product's own fix: the editor opens on a real starting spec, and a
  // row of chips swaps in the other worked ones (policy-panel.tsx's
  // POLICY_TEMPLATES, seeded from examples/policies/*.json). "Package
  // registries" is the pick because it VISIBLY rewrites the document — the
  // dialog already prefills with Minimal, so a Minimal click would be a dead
  // one — and because its own floor is CC2, the floor this policy keeps.
  const templateRow = dlg.getByText("Start from a template");
  await spotlight(page, templateRow);
  // DIALOG-NEW-BEAT: the chips have no line in the owner's script — they did
  // not exist when it was written. Drafted; see
  // local/heavy-episodes-dialog-proposals.md.
  await caption(page, "Start from a template.");
  await beat(page, BEAT_SHORT);
  // DIALOG-NEW-BEAT (dialog review, A7): the chips are the episode's own
  // answer to "how do I start" and the line names all five. Asserted, because
  // narration that enumerates a UI list must not outlive it. Drafted; see
  // local/heavy-episodes-dialog-proposals.md.
  for (const label of ["Minimal", "Model provider only", "Package registries", "CI baseline", "Allow-all — observe first"]) {
    await expect(
      dlg.getByRole("button", { name: label }),
      `no "${label}" template chip — the chips line names five, and this take would speak one that is gone`,
    ).toBeVisible();
  }
  await caption(
    page,
    "And you don't start from a blank page. Minimal. Model provider only. Package registries. A CI baseline. Or allow-all, if you'd rather observe first and tighten later.",
  );
  await beat(page, PACE.read);
  await act(page, dlg.getByRole("button", { name: "Package registries" }));

  // PASTE, never keystroke over the template — friction (3) in the file
  // header. fill() replaces the textarea's whole value in one call,
  // functionally identical to a clipboard paste, so no intermediate
  // half-edited spec is ever on screen.
  const specBox = dlg.getByLabel("Spec (JSON)");
  await spotlight(page, specBox);
  await specBox.fill(JSON.stringify(POLICY_SPEC, null, 2));

  // The panel's LIVE derivations, re-read on every keystroke — so the paste
  // is proven here, on camera, instead of three beats later on the saved
  // row: the template put eleven hosts in this chip a moment ago.
  const egressChip = dlg.getByText(POLICY_EGRESS_SUMMARY, { exact: true });
  await expect(
    egressChip,
    "the panel does not read '2 domains allowed' — the paste never replaced the template",
  ).toBeVisible();
  await spotlight(page, egressChip);
  await caption(page, "We'll give this one two allowed hosts.");
  await beat(page, PACE.read);
  await spotlight(page, specBox);
  await caption(page, "Everything else should require a decision.");
  await beat(page, PACE.read);

  // SCREEN: Policy JSON — the field walk, which now has somewhere to point.
  // The panel's helper rail documents every key of RunPolicySpec with its
  // legal values (policy-panel.tsx's FIELD_HELP), so the owner's two "this
  // setting / this one" lines land on the two fields they are about. Each row
  // is located by its own "Insert <key>" button (a unique aria-label) rather
  // than the key text, which also appears inside the textarea above.
  const railEntry = (key: string) =>
    dlg.getByRole("button", { name: `Insert ${key}` }).locator("xpath=ancestor::li[1]");
  const firstUse = railEntry("first_use_approval");
  await centerInFrame(firstUse); // S2: the dialog scrolls; keep it off the caption bar
  await spotlight(page, firstUse);
  await caption(page, "This setting controls that behavior.");
  await beat(page, PACE.read);
  const floorField = railEntry("min_confinement_class");
  await centerInFrame(floorField);
  await spotlight(page, floorField);
  await caption(page, "And this one sets the minimum confinement level.");
  await beat(page, PACE.read);
  // The floor BADGE — the panel's own derived chip, not the raw JSON.
  await spotlight(page, dlg.getByText(POLICY_FLOOR_LABEL, { exact: true }).first());
  await caption(page, "Think of it as the floor.");
  await beat(page, BEAT_SHORT);
  await caption(page, "The policy can require a stronger barrier than the machine's default.");
  await beat(page, PACE.read);
  // The plan said "Set it to Wall, and this run refuses to start here." Wall is
  // installed on this host, so that sentence is false here — and any tier we
  // named instead could go false the next time a runtime is added or removed.
  // The RULE is what is always true (resolveEnforcedConfinement, runs.go:153:
  // the runner must advertise the exact enforced class, or the create fails).
  await caption(page, "But it can't ask a machine to provide a barrier it doesn't support.");
  await beat(page, PACE.read + 400);
  await spotlight(page, null);

  await act(page, dlg.getByRole("button", { name: "Create policy" }), "Create.");
  await expect(dlg).toBeHidden({ timeout: 30_000 });

  const row = page.getByRole("row", { name: new RegExp(POLICY_NAME) });
  await expect(row).toBeVisible({ timeout: 30_000 });

  // THE PASTE, PROVEN — by the egress chip now: if .fill() had silently left
  // the starter behind, this row would read "1 domain allowed" (STARTER_SPEC
  // allows one host), not two. The barrier chip alone no longer tells the two
  // specs apart — STARTER_SPEC also floors at CC2 (friction 3) — but it still
  // confirms the saved row matches what we authored, so both stay asserted.
  await expect(
    row.getByText(POLICY_FLOOR_LABEL, { exact: true }),
    "the saved policy's barrier floor is not Wall — it does not match what B2 pasted",
  ).toBeVisible();
  await expect(
    row.getByText(POLICY_EGRESS_SUMMARY, { exact: true }),
    "not '2 domains allowed' — the CC2 starter spec's one-host egress was saved instead of the pasted one",
  ).toBeVisible();

  // ID column: Name(0), ID(1), Barrier(2), Egress(3), Attributes(4), Updated(5)
  // — policies.tsx:208-215's own column order.
  createdPolicyId = (await row.getByRole("cell").nth(1).textContent())?.trim() ?? "";
  expect(createdPolicyId, "policy row rendered but its ID cell read empty").not.toBe("");

  // THE RECEIPT, HELD. All four personas called the bare UUID "a receipt in a
  // language I wasn't given a dictionary for" — nothing here can translate it
  // (there is no name beyond the one already on the row), so the fix is to
  // hold on it long enough to read, and pay it off later at the run page.
  await spotlight(page, row);
  await caption(page, "Saved.");
  await beat(page, BEAT_SHORT);
  await caption(page, "And now this policy has an identity.");
  await beat(page, PACE.read);
  await caption(page, "Every run that uses it can point back to the exact rules that governed it.");
  await beat(page, PACE.read + 400);
  await spotlight(page, null);
});

// ---------------------------------------------------------------------------
// B3 — use it
// ---------------------------------------------------------------------------

test("B3 — use it", async () => {
  test.setTimeout(180_000);
  const page = stage();

  await act(page, page.locator("header").getByRole("button", { name: "New run" }), "New run.");
  await expect(page).toHaveURL(/\/runs\/new/, { timeout: 30_000 });

  const titleBox = page.getByLabel("Title");
  await spotlight(page, titleBox);
  await titleBox.fill(DEMO_RUN_TITLE);
  await spotlight(page, null);

  // KEYLESS (file header): a Shell command needs no agent, no model and no
  // Credentials-rail warning. The wizard's default is an interactive AGENT run,
  // so this click swaps the "Run type" segment and the Task field becomes
  // "Command".
  await act(page, page.getByRole("radio", { name: "Shell command" }), "Shell command.");
  const cmdBox = page.getByLabel("Command");
  await spotlight(page, cmdBox);
  await cmdBox.fill(DEMO_COMMAND);
  await spotlight(page, null);

  // "Saved policy" is the Policy panel's MODE ROW now, not a radio: an
  // OptionCard, which is an aria-pressed <button> whose accessible name is
  // its title plus its hint line — so prefix-match the title (same shape, and
  // the same trap, as the Add-workspace dialog's cards). The picker itself
  // only renders once this half of the row is lit.
  await act(page, page.getByRole("button", { name: /^Reuse a saved policy/ }), "Select saved policy.");
  await act(page, page.getByRole("combobox", { name: "Saved policy" }));
  await act(page, page.getByRole("option", { name: POLICY_NAME }));

  // FRICTION (2), NOW FIXED — and this is the beat the plan could not have: the
  // rail no longer describes screen state a saved-policy launch would drop. It
  // names the STORED policy, its own floor and its own host count, and says
  // outright that nothing on this page is merged into it. Scoped to the rail
  // (the sidebar is an <aside> too; only this one carries its heading), because
  // the policy's name is also showing in the Select trigger to the left.
  const rail = page.locator("aside").filter({ hasText: "What this run can do" });
  await expect(rail.getByText(POLICY_NAME, { exact: true })).toBeVisible();
  const railPolicy = rail.getByText(/^The stored spec governs this run/);
  await expect(railPolicy).toBeVisible();
  // The stored spec's OWN facts, echoed by the rail — not the screen's. Not
  // narrated directly (the owner's B3 line stays at the higher-level
  // distinction below), but still proven: a claim silently dropped from the
  // rail would otherwise ship undetected.
  await expect(railPolicy).toContainText(`barrier floor ${POLICY_FLOOR_LABEL}`);
  await expect(railPolicy).toContainText("2 hosts allowed");
  await expect(railPolicy).toContainText("It launches by reference, so nothing on this page is merged into it.");

  await spotlight(page, rail);
  // DIALOG-NEW-BEAT (dialog review, A5): the reuse-by-reference the rail is
  // already proving above has no line on it — the episode's premise ("write
  // the rules once") pays off HERE, not at the paste. Anchored to this beat
  // because this is where the driver films it. Drafted; see
  // local/heavy-episodes-dialog-proposals.md.
  await caption(page, "Here's the same set of rules we built by hand in episode seven — saved once, named, and reusable.");
  await beat(page, PACE.read);
  await caption(page, "Now a new run just points at that policy.");
  await beat(page, BEAT_SHORT);
  await caption(page, "And here's the important distinction.");
  await beat(page, PACE.read);
  // The left column now shows the PICKER in place of the spec textarea — the
  // panel hides the document it isn't going to send — while the Barrier Seg
  // stays live below it, because the run's requested class is a separate wire
  // field from the policy's floor. B4 films exactly where those two meet.
  const barrierRail = rail.getByText("Barrier", { exact: true }).locator("xpath=..");
  await spotlight(page, barrierRail);
  await caption(page, "The sandbox is the wall around the workload.");
  await beat(page, PACE.read);
  await spotlight(page, railPolicy);
  await caption(page, "The policy is the set of rules governing what happens around that wall.");
  await beat(page, PACE.read);
  await caption(page, "The policy brings those rules with it.");
  await beat(page, PACE.read + 400);
  await spotlight(page, null);
});

// ---------------------------------------------------------------------------
// B4 — launch, effective policy
// ---------------------------------------------------------------------------

test("B4 — launch, effective policy", async () => {
  test.setTimeout(600_000);
  const page = stage();

  const rail = page.locator("aside").filter({ hasText: "What this run can do" });
  // The rail's launch-error line (new-run-screen.tsx's `error &&` paragraph),
  // which renders the control plane's own message verbatim. Raced against the
  // navigate below so a refused launch fails in a second with a cause, instead
  // of a 3-minute URL timeout at the end of an otherwise-finished take.
  // `.first()`: the rail grew a SECOND danger-toned paragraph for preflight
  // errors, and this video never clicks Preflight — DOM order keeps the launch
  // one, and a strict-mode violation can no longer happen if it ever does.
  const launchError = rail.locator("p.text-danger").first();

  // ── THE FLOOR, FILMED ─────────────────────────────────────────────────
  // The one claim in eight videos delivered without footage, per all four
  // personas — and it is now a STANDING refusal instead of a race. The
  // Barrier Seg disables every tier below the attached policy's floor and
  // prints its own reason line for each one (new-run-screen.tsx's floor
  // block), so Fence is greyed the moment B3 picked nightly-triage. The old
  // detach-on-edit that used to make this a two-branch gamble is GONE: the
  // spec text is the only thing that detaches a policy now.
  //
  // So this beat RINGS Fence rather than clicking it. Clicking a disabled
  // button would park act() on it for the full 45s action timeout, and there
  // would be nothing to see at the end of the wait.
  const barrier = page.getByRole("radiogroup", { name: "Barrier" });
  const fenceRadio = barrier.getByRole("radio", { name: "Fence" });
  await centerInFrame(fenceRadio);
  await spotlight(page, fenceRadio);
  // DIALOG-STALE(no click left to make): the owner's SAY-ON-CLICK "Select
  // Fence." and this line both narrated PICKING the weaker tier. The form no
  // longer offers it while a CC2-floored policy is attached — the refusal is
  // now visible before the click instead of after it. The "Select Fence."
  // click line is dropped (no control to click); this line rides the ring on
  // the disabled tier. See local/heavy-episodes-dialog-proposals.md.
  await caption(page, "Now we'll deliberately choose a barrier below the policy's minimum.");
  await beat(page, PACE.read);
  await expect(
    fenceRadio,
    "Fence is selectable beneath the policy's CC2 floor — the floor is not being enforced on the form",
  ).toBeDisabled();

  // The Seg's OWN reason line for a floor-disabled tier — distinct from the
  // "isn't installed on this host" line, which would be a lie about a tier
  // this host builds fine. Hardcoded labels, same rule as the file header:
  // both are OUR policy's floor and OUR choice of tier, never a claim about
  // the host.
  const floorReason = page.getByText(`Fence is below the policy's floor (${POLICY_FLOOR_LABEL}).`);
  await expect(
    floorReason,
    "the Seg disables Fence without saying why — the floor is enforced but not explained",
  ).toBeVisible();
  await spotlight(page, floorReason);
  // DIALOG-STALE(nothing is started, and nothing is refused at launch): the
  // refusal happens on the form now, before any request exists.
  await caption(page, "And Wardyn refuses to start it.");
  await beat(page, PACE.read);
  await caption(page, "The policy requires a stronger floor.");
  await beat(page, PACE.read);
  // DIALOG-STALE(the control plane is never asked): the run below the floor
  // cannot be composed, so no 422 is ever raised. The server check still
  // exists (runs_create.go) — it is simply no longer what the viewer sees.
  await caption(page, "So the control plane says no.");
  await beat(page, PACE.read + 400);
  await spotlight(page, null);

  // The owner's next line, and now a REAL click: Wall is the policy's own
  // floor and the weakest tier the Seg still offers. It is also what pins
  // this run — picking a policy only ever UP-clamps the requested class, so
  // without this click the run would launch at whatever this host defaults
  // to (Vault today) and the floor would never have bitten on camera.
  await act(page, barrier.getByRole("radio", { name: POLICY_FLOOR_LABEL }), "Now choose a barrier that meets the floor.");
  // The barrier is a separate wire field from the policy's floor, so changing
  // it must NOT detach the policy — the whole reason the old detach died.
  await expect(
    rail.getByText(POLICY_NAME, { exact: true }),
    "the policy detached when the barrier changed — this run would launch inline, not by reference",
  ).toBeVisible();

  // SPRINT: nothing spoken between here and the wait, because everything from
  // here IS the wait.
  await act(page, page.getByRole("button", { name: "Launch run" }), "Launch.");

  // FAST-FORWARD. POST /runs dispatches SYNCHRONOUSLY (runs_dispatch.go: "dispatch
  // is invoked synchronously from the create-run handler"), so the navigate does
  // not happen until the container is provisioned — the URL change, the cockpit
  // mount and the command's own few seconds are ONE stretch of dead air. act()
  // above already held for its own pacing; the beat(200) drains any residual
  // speech, because opening a span over a still-speaking caption puts that
  // speech inside compressed footage and lands every later cue early.
  await beat(page, 200);
  await ffwdStart(page);
  try {
    await waitUnlessGone(
      expect(page).toHaveURL(/\/runs\/[0-9a-f-]{8,}/i, { timeout: SANDBOX_UP }),
      launchError,
      SANDBOX_UP,
      "the run never reached its cockpit",
      "the control plane REFUSED the launch and the rail is showing why",
    );
    // Wait for the run to actually finish — see the file header's geometry note.
    // Raced against the three ways it could end badly, so a broken run fails by
    // NAME here rather than being narrated over as if it had worked.
    await waitUnlessGone(
      // .first() on both sides: the state badge is the FIRST thing in the
      // command bar, so DOM order picks it even if a widget ever renders the
      // same word further down the cockpit.
      expect(page.locator("#main-content").getByText(RUN_DONE, { exact: true }).first()).toBeVisible({
        timeout: SANDBOX_UP,
      }),
      page.locator("#main-content").getByText(RUN_BROKE).first(),
      SANDBOX_UP,
      "the governed run never completed",
      "it reached a FAILED/STOPPED/KILLED state instead",
    );
  } finally {
    // Real time resumes the instant the run has settled — and even on a failed
    // take, so the span the encoder gets is one this run actually spent.
    await ffwdEnd(page);
  }

  await caption(page, "This time it runs.");
  await beat(page, PACE.read);

  // THE PAYOFF, READ OFF THE WIRE FIRST. Everything narrated below is a claim
  // about what the CONTROL PLANE recorded, so it is checked there before it is
  // checked on screen: this run must carry OUR policy's id, not some inline or
  // default policy that only coincidentally also runs at the same barrier.
  const runId = page.url().split("/runs/")[1]?.split(/[?#]/)[0] ?? "";
  const runRes = await page.request.get(`/api/v1/runs/${runId}`, { headers: apiHeaders() });
  expect(runRes.ok(), `GET /api/v1/runs/${runId} failed (${runRes.status()})`).toBe(true);
  const run = await runRes.json();
  expect(
    run.policy_id,
    "the launched run carries no policy_id (or the wrong one) — it did not launch by reference",
  ).toBe(createdPolicyId);

  // The barrier the control plane ENFORCED, named by the server's own map —
  // read off the wire rather than hardcoded. Expect "Wall": the floor beat
  // above clicked Wall explicitly, and the Seg cannot go below the attached
  // policy's floor, so this run's requested class is pinned to CC2 by
  // construction now, not by whatever this host's default happens to be
  // (file header). The assertion still reads whatever the server says.
  const enforcedLabel = hostNames[run.confinement_class] ?? String(run.confinement_class);
  // #main-content excludes the app-shell's OWN top-bar barrier chip, which shows
  // the host's strongest tier on every route — without this scope the run
  // header's chip is not the only match.
  const main = page.locator("#main-content");
  const headerBarrier = main.getByText(enforcedLabel, { exact: true }).first();
  await expect(headerBarrier).toBeVisible({ timeout: 30_000 });

  await caption(page, "And now the record tells us two things:");
  await beat(page, PACE.read);
  await spotlight(page, headerBarrier);
  await caption(page, "which barrier actually ran...");
  await beat(page, PACE.read);

  // The Identity widget's Policy row (identity.tsx:78-85) renders only when the
  // run HAS a policy_id — which is exactly the claim. Both cockpit presets place
  // the widget (widget-registry.ts:155-162), and this run is terminal by now, so
  // it is the `finished` one; spotlight() scrolls the row into view inside the
  // tile's own overflow container. The ring flies straight from the header
  // chip to this row — both are on screen at once, and that flight IS "two
  // things".
  const policyRow = main.getByText(createdPolicyId, { exact: true });
  await expect(policyRow).toBeVisible({ timeout: 30_000 });
  await spotlight(page, policyRow);
  await caption(page, "and which policy governed it.");
  await beat(page, PACE.read);

  await caption(page, "That's the important part.");
  await beat(page, PACE.read);
  await caption(page, "Not what we happened to select on the form.");
  await beat(page, PACE.read);
  await caption(page, "What the control plane actually enforced.");
  await beat(page, PACE.read + 400);
  await spotlight(page, null);

  await caption(page, "One policy.");
  await beat(page, BEAT_SHORT);
  await caption(page, "Every run that uses it gets those same rules.");
  await beat(page, PACE.read + 600);

  // ---- Conclusion ------------------------------------------------------------
  await caption(page, "We've gone from manually configuring every run...");
  await beat(page, PACE.read);
  await caption(page, "to writing the rules once.");
  await beat(page, PACE.read);
  await caption(page, "But there's still a problem.");
  await beat(page, BEAT_SHORT);
  await caption(page, "How do you know what the right rules should be in the first place?");
  await beat(page, PACE.read);
  await caption(page, "That's what episode nine is about.");
  await beat(page, PACE.read + 400);
  await silentCard(page, "Next — 09: Record a run into a policy");
});

/**
 * A chapter card that is NOT spoken — the outro card convention episode 01
 * set (and episode 03 copies). overlay.ts's chapter() always speaks what it
 * renders; this drives the same overlay primitive directly for the one card
 * that must stay silent.
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
}
