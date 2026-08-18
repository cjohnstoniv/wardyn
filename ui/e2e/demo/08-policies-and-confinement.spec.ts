/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * Video 8 of the 0.5 series — Policies & confinement tiers.
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
 * so all three columns read "Ready", the top-bar chip is VAULT (not Fence), and
 * the New Run wizard's own resolveDefaultCc(getDefaultCc(), classes) picks the
 * STRONGEST available class — this run launches at Vault, above the policy's
 * Fence floor.
 *
 * So nothing below names a tier or a status the host might contradict:
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
 *     confinement_names map. No tier name is spoken.
 * The one label this file DOES hardcode is "Fence" — as the floor of the policy
 * IT AUTHORS (min_confinement_class: CC1). That is our own spec, not the host's.
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
 * FRICTION NOTES, RE-VERIFIED AGAINST SOURCE (2026-08-18). Three of the plan's
 * four Track-A findings for this video are FIXED on this branch:
 *   (1) FIXED — new-run-screen.tsx:355-366: the launch payload is gated on the
 *       confinement RADIO (`confinement === "saved" && state.selectedPolicyId`),
 *       not a stale selectedPolicyId alone; patch() also detaches the id on any
 *       envelope edit (:243-258).
 *   (2) FIXED — the rail HAS a Policy section now (:809-822): it names the
 *       stored policy, its barrier floor and its host count, and hides the
 *       wizard's own Network section beneath it ("The network edits on this
 *       page do not apply to it."). The plan's B3 camera direction — "camera
 *       stays on the select, rail does NOT reflect saved policies today" — is
 *       therefore OBSOLETE, and B3 below spotlights the rail instead.
 *   (3) STILL LIVE — policies.tsx:70-75's STARTER_SPEC floors at CC2. B2 never
 *       edits it: it PASTES a prepared CC1 spec over the whole textarea with
 *       .fill(), which replaces the value in one call exactly like a real
 *       clipboard paste. B2 then asserts the SAVED row's barrier chip reads
 *       Fence — if the paste ever silently failed, the starter's Wall would be
 *       on the row and the take dies there rather than three beats later.
 *   (4) FIXED — new-run-screen.tsx:281 re-resolves the persisted default
 *       against the host's REAL class list, so Settings' "saved as your default
 *       barrier" promise now holds here too. It is also why this run lands at
 *       the strongest tier rather than CC1 (see the block above).
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

import { test, expect, type Locator } from "@playwright/test";
import { act, beat, caption, chapter, ffwdEnd, ffwdStart, PACE, spotlight } from "./overlay";
// stage.ts is the rig: importing it registers this file's beforeAll/afterAll
// (one browser, one context, one recorded page), and every beat reads the page
// out of stage() inside a test body rather than closing over a module binding.
import { stage } from "./stage";

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
 * The prepared spec B2 PASTES. Four fields, matching the SAY line "The spec is
 * four fields." exactly — and min_confinement_class is CC1 ON PURPOSE: this is
 * the counter-example to policies.tsx's CC2-floored STARTER_SPEC (friction 3),
 * which is never edited in place.
 */
const POLICY_SPEC = {
  allowed_domains: ["github.com", "npmjs.org"],
  first_use_approval: "wait_for_review",
  min_confinement_class: "CC1",
  eligible_grants: [],
} as const;

/**
 * How the SAVED policy's own fields must render — OUR spec, so these are
 * host-independent and safe to hardcode (unlike anything about the host's
 * barriers). "Fence" is CC_META.CC1.label (cc-meta.ts:46); "2 domains allowed"
 * is egressSummary()'s exact phrasing (policies.tsx:87-91).
 */
const POLICY_FLOOR_LABEL = "Fence";
const POLICY_EGRESS_SUMMARY = "2 domains allowed";

/**
 * B3's run. A Shell command, not an Agent task — see the file header's KEYLESS
 * note. The command does nothing that could fail: this video is not about what
 * the run DOES, only about which policy governed it.
 */
const DEMO_RUN_TITLE = "Nightly triage, governed by policy";
const DEMO_COMMAND = 'echo "nightly triage: running under the saved policy"';

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
 * Delete any policy already named POLICY_NAME, and read the host's own barrier
 * truth — both off camera, before the first beat.
 *
 * The delete is per-take hygiene: a stale `nightly-triage` left by a prior
 * --no-reset take makes B2's getByRole("row", …) a strict-mode violation (two
 * matches), which fails confusingly halfway through rather than here. It is
 * also what makes this video immediately re-takeable: the second take is
 * byte-identical to the first.
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

  await chapter(page, "Policies & confinement", "Governance you configure once, and every later run inherits");
  await caption(page, "Every run so far, you configured by hand.");
  await beat(page, PACE.read);
  await caption(page, "A policy makes governance reusable, and reviewable.");
  await beat(page, PACE.read);

  // Silent nav — the SAY lines below narrate what is now on screen, not the
  // click that got here. The chip's aria-label is the STRONGEST-available form
  // (app-shell.tsx:543); the "No sandbox barrier" variant is impossible after
  // beforeAll's assertion.
  await act(page, page.getByRole("link", { name: "Sandbox barrier — open Settings" }));
  await expect(page.getByRole("heading", { name: "Host", level: 3 })).toBeVisible({ timeout: 30_000 });

  const matrix = page.getByRole("radiogroup", { name: "Barrier tier" });
  await expect(matrix).toBeVisible({ timeout: 30_000 });

  await caption(page, "This card names the barriers this machine can actually build.");
  await spotlight(page, page.getByRole("heading", { name: "Host", level: 3 }));
  await beat(page, PACE.read);
  await spotlight(page, null);

  // THE HONESTY BEAT, said WITHOUT a verdict. The plan's line here named a
  // specific three-way answer ("Fence, ready. Wall, needs setup. Vault,
  // incompatible here") that this host contradicts today — see the file header.
  // The vocabulary is the teachable part and is true on every host; the verdict
  // is asserted below against what the host itself reports.
  await caption(page, "Fence, Wall, Vault — and each column says whether this machine can build it.");

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
  await beat(page, PACE.read + 400);
  await spotlight(page, null);

  await caption(page, "Fence shares your kernel. Wall gives the agent a software kernel. Vault, its own machine.");
  await spotlight(page, page.getByRole("row", { name: /^Mechanism/ }));
  await beat(page, PACE.read + 400);
  await spotlight(page, null);

  await caption(page, "Wardyn never claims a barrier it cannot enforce.");
  await beat(page, PACE.read);
});

// ---------------------------------------------------------------------------
// B2 — create a policy
// ---------------------------------------------------------------------------

test("B2 — create a policy", async () => {
  test.setTimeout(180_000);
  const page = stage();

  await act(page, page.getByRole("link", { name: "Policies" }));
  await expect(page.getByRole("heading", { name: "Policies", level: 1 })).toBeVisible({ timeout: 30_000 });

  // The operator gate, named. A member's button is disabled (policies.tsx:141)
  // and act() would simply park on it for the full 45s action timeout, which on
  // camera is indistinguishable from the app hanging.
  const newPolicyBtn = page.getByRole("button", { name: "New policy" });
  await expect(
    newPolicyBtn,
    "the New policy button is disabled — this session is not an operator (shoot in local mode)",
  ).toBeEnabled({ timeout: 30_000 });

  await act(page, newPolicyBtn);
  const dlg = page.getByRole("dialog");
  await expect(dlg.getByRole("heading", { name: "New policy" })).toBeVisible();

  await caption(page, "Name it. The spec is four fields.");
  const nameBox = dlg.getByLabel("Name");
  await spotlight(page, nameBox);
  await nameBox.fill(POLICY_NAME);
  await spotlight(page, null);
  await beat(page, PACE.read);

  // PASTE, never edit the CC2-floored STARTER_SPEC in place — friction (3) in
  // the file header. fill() replaces the textarea's whole value in one call,
  // functionally identical to a clipboard paste; it never keystrokes over the
  // prefilled starter, so no intermediate half-edited spec is ever on screen.
  const specBox = dlg.getByLabel("Spec (JSON)");
  await spotlight(page, specBox);
  await specBox.fill(JSON.stringify(POLICY_SPEC, null, 2));
  await beat(page, PACE.read);

  await caption(page, "Two hosts allowed. Anything unlisted is held for your approval.");
  await beat(page, PACE.read);
  await caption(page, "Min confinement class is the floor — the weakest barrier this policy accepts.");
  await beat(page, PACE.read);
  // The plan said "Set it to Wall, and this run refuses to start here." Wall is
  // installed on this host, so that sentence is false here — and any tier we
  // named instead could go false the next time a runtime is added or removed.
  // The RULE is what is always true (resolveEnforcedConfinement, runs.go:153:
  // the runner must advertise the exact enforced class, or the create fails).
  await caption(page, "Raise the floor past what a host can build, and the run refuses to start there.");
  await beat(page, PACE.read + 500);
  await spotlight(page, null);

  await act(page, dlg.getByRole("button", { name: "Create policy" }));
  await expect(dlg).toBeHidden({ timeout: 30_000 });

  const row = page.getByRole("row", { name: new RegExp(POLICY_NAME) });
  await expect(row).toBeVisible({ timeout: 30_000 });

  // THE PASTE, PROVEN. If .fill() had ever silently left the starter behind,
  // this row's barrier chip would read Wall (STARTER_SPEC's CC2) and its egress
  // chip "1 domain allowed" — and the run in B4 would be governed by a policy
  // the narrator never described. Both are OUR spec's own values, so neither
  // depends on what this host can build.
  await expect(
    row.getByText(POLICY_FLOOR_LABEL, { exact: true }),
    "the saved policy's barrier floor is not Fence — the CC2 starter spec was saved instead of the pasted one",
  ).toBeVisible();
  await expect(row.getByText(POLICY_EGRESS_SUMMARY, { exact: true })).toBeVisible();

  // ID column: Name(0), ID(1), Barrier(2), Egress(3), Attributes(4), Updated(5)
  // — policies.tsx:208-215's own column order.
  createdPolicyId = (await row.getByRole("cell").nth(1).textContent())?.trim() ?? "";
  expect(createdPolicyId, "policy row rendered but its ID cell read empty").not.toBe("");
});

// ---------------------------------------------------------------------------
// B3 — use it
// ---------------------------------------------------------------------------

test("B3 — use it", async () => {
  test.setTimeout(180_000);
  const page = stage();

  await act(page, page.locator("header").getByRole("button", { name: "New run" }));
  await expect(page).toHaveURL(/\/runs\/new/, { timeout: 30_000 });

  const titleBox = page.getByLabel("Title");
  await spotlight(page, titleBox);
  await titleBox.fill(DEMO_RUN_TITLE);
  await spotlight(page, null);

  // KEYLESS (file header): a Shell command needs no agent, no model and no
  // Credentials-rail warning. Deliberately SILENT — it is the one SCREEN action
  // the plan's own B3 line does not narrate, the same convention B4 uses for the
  // cut "Launch." caption. The wizard's default is an interactive AGENT run, so
  // this click swaps the "Run type" segment and the Task field becomes "Command".
  await act(page, page.getByRole("radio", { name: "Shell command" }));
  const cmdBox = page.getByLabel("Command");
  await spotlight(page, cmdBox);
  await cmdBox.fill(DEMO_COMMAND);
  await spotlight(page, null);

  await act(
    page,
    page.getByRole("radio", { name: /^Saved policy/ }),
    "New run, Confinement, Saved policy — pick it.",
  );
  await act(page, page.getByRole("combobox", { name: "Saved policy" }));
  await act(page, page.getByRole("option", { name: POLICY_NAME }));

  await caption(page, "Barrier is the wall. Confinement is the promise. The policy carries both.");

  // FRICTION (2), NOW FIXED — and this is the beat the plan could not have: the
  // rail no longer describes wizard state a saved-policy launch would drop. It
  // names the STORED policy, its own floor and its own host count, and says
  // outright that this page's network edits do not apply. Scoped to the rail
  // (the sidebar is an <aside> too; only this one carries its heading), because
  // the policy's name is also showing in the Select trigger to the left.
  const rail = page.locator("aside").filter({ hasText: "What this run can do" });
  await expect(rail.getByText(POLICY_NAME, { exact: true })).toBeVisible();
  const railPolicy = rail.getByText(/^The stored spec governs this run/);
  await expect(railPolicy).toBeVisible();
  // The stored spec's OWN facts, echoed by the rail — not the wizard's.
  await expect(railPolicy).toContainText(`barrier floor ${POLICY_FLOOR_LABEL}`);
  await expect(railPolicy).toContainText("2 hosts allowed");
  await expect(railPolicy).toContainText("The network edits on this page do not apply to it.");
  await spotlight(page, railPolicy);
  await beat(page, PACE.read + 600);
  await spotlight(page, null);
});

// ---------------------------------------------------------------------------
// B4 — launch, effective policy
// ---------------------------------------------------------------------------

test("B4 — launch, effective policy", async () => {
  test.setTimeout(600_000);
  const page = stage();

  // The rail's launch-error line — the only danger-toned paragraph on this
  // screen (new-run-screen.tsx:933-938), and it renders the control plane's own
  // message verbatim. Raced against the navigate below so a refused launch
  // fails in a second with a cause, instead of a 3-minute URL timeout at the
  // end of an otherwise-finished take.
  const launchError = page
    .locator("aside")
    .filter({ hasText: "What this run can do" })
    .locator("p.text-danger");

  // SV15: the click plays silent — "Launch." was cut from the script; the lines
  // after the boot carry the beat instead. SPRINT: nothing is spoken between
  // here and the wait, because everything from here IS the wait.
  await act(page, page.getByRole("button", { name: "Launch run" }));

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

  // The barrier the control plane ENFORCED, named by the server's own map. Not
  // "Fence": on a host that can build more, the wizard resolves the strongest
  // available class and the policy's CC1 is only the floor (file header).
  const enforcedLabel = hostNames[run.confinement_class] ?? String(run.confinement_class);
  // #main-content excludes the app-shell's OWN top-bar barrier chip, which shows
  // the host's strongest tier on every route — without this scope the run
  // header's chip is not the only match.
  const main = page.locator("#main-content");
  const headerBarrier = main.getByText(enforcedLabel, { exact: true }).first();
  await expect(headerBarrier).toBeVisible({ timeout: 30_000 });
  await caption(page, "The barrier in the header. The policy it ran under, on Identity.");
  await spotlight(page, headerBarrier);
  await beat(page, PACE.read);
  await spotlight(page, null);

  // The Identity widget's Policy row (identity.tsx:78-85) renders only when the
  // run HAS a policy_id — which is exactly the claim. Both cockpit presets place
  // the widget (widget-registry.ts:155-162), and this run is terminal by now, so
  // it is the `finished` one; spotlight() scrolls the row into view inside the
  // tile's own overflow container.
  const policyRow = main.getByText(createdPolicyId, { exact: true });
  await expect(policyRow).toBeVisible({ timeout: 30_000 });
  await caption(page, "Not what you typed. What the control plane enforced.");
  await spotlight(page, policyRow);
  await beat(page, PACE.read);
  await spotlight(page, null);

  // ---- Outro ---------------------------------------------------------------
  await caption(page, "One policy. Every run after it, already governed.");
  await beat(page, PACE.read + 600);
  // The plan's outro said "Next: Record Mode" under the OLD numbering, where
  // this was V06. Under the restructure this is 08 and Record Mode already
  // shipped as 06 — 09 is ci-and-headless, so the hand-off goes there.
  await caption(page, "Next: the same guarantees with nobody watching — Wardyn inside a CI pipeline.");
  await beat(page, PACE.chapter);
  await caption(page, "");
});
