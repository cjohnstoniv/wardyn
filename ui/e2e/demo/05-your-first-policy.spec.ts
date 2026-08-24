/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * Video 05 of the series — "Your first policy".
 *
 * WHAT THIS FILMS. The policy editor, end to end, with the SAFETY METER as the
 * centerpiece. Four acts:
 *   Act 1 — the panel and its templates. Open /policies, "New policy", the row
 *           of template chips (Minimal / Model provider only / Package
 *           registries / CI baseline / Allow-all), the readable JSON, and the
 *           helper rail that documents every key of a policy spec.
 *   Act 2 — THE SAFETY METER (the reason this episode exists). The meter grades
 *           the DOCUMENT as written (POST /policies/grade, composer.Grade,
 *           debounced) and its label is filmed MOVING across the whole range:
 *             click Minimal            -> "Guarded"   (CC2 = one medium)
 *             hand-edit floor to CC3   -> "Safest"    (all low)
 *             hand-edit floor to CC1   -> "Elevated"  (one high item)
 *             click Allow-all          -> "Weakest"   (two high items)
 *           A single field edit can only ever add ONE high, so the fourth step
 *           is the allow-all TEMPLATE click (allow-all egress + never-reap =
 *           the second high) — that is why the ramp is three edits plus one
 *           template click, never four edits. Every label is ASSERTED via the
 *           meter's data-safety attribute (safety-meter.tsx), re-derived so all
 *           four are reachable on camera (round-2 H-2, round-4 M-2 of the plan).
 *           The meter's own title — "Safety of the policy document as written" —
 *           is surfaced too: it grades the DOCUMENT, a different question than a
 *           run's Preflight of the RESOLVED run.
 *   Act 3 — the confinement floor. The min_confinement_class field ("the run
 *           refuses to launch below it"), the derived "Wall" floor badge, and
 *           the barrier-tier callback — all TAUGHT in the editor.
 *   Act 4 — save, then reuse. Name it, Create it (operator-gated), read its new
 *           identity off the row, then hop to the New Run form to show the
 *           policy reused BY REFERENCE and the floor REFUSING every barrier
 *           below it (Fence disabled beneath the Wall floor, the Seg's own
 *           reason line). It STOPS before launch — launching this run is 06.
 *
 * ── PROVENANCE: the retiring-08 lift ───────────────────────────────────────
 * The confinement-floor teaching, the create/save beat, the wall-vs-rules
 * distinction, and the floor-refuses beat are LIFTED VERBATIM from old-08
 * (ui/e2e/demo/retiring-policies-and-confinement.spec.ts, whose material this
 * episode absorbs per Workstream C of /home/cjohn/.claude/plans/merry-snacking-
 * harbor.md). Owner lines MOVE unchanged; the safety-meter beats, the panel/
 * template intro, the forward-looking reuse teaching, and the conclusion are
 * NEW, marked [OWNER SLOT — drafted] in local/episode-05-policy-proposal.md.
 * The three B4 floor lines carry old-08's own DIALOG-STALE note (the refusal is
 * a STANDING form refusal now, before any launch request exists) — kept exactly
 * as old-08 shipped them, because new-05 films the identical on-form refusal.
 * Two owner lines the series RE-ORDER reframed were re-drafted rather than
 * lifted: the modal-body "A policy is a spec…" (old-08 back-referenced a spec
 * from old-07; new-05 has no such prior) and the reuse teaching (old-08's was
 * BACKWARD-looking; new-05 runs BEFORE the first run, so it is FORWARD-looking).
 *
 * THE SAVED POLICY IS NAMED "first-policy" — new-06's lane MUST reference this
 * exact noun when it reworks "your first run" to reuse-by-reference.
 *
 * KEYLESS, no launch. This episode never starts a sandbox: it authors a policy
 * and previews the floor on the form, then stops. No model, no quota, no
 * workspace dependency, no host-capability probe (the floor's disable is driven
 * by the POLICY floor, not the host — unlike old-08, which clicked Wall for real
 * and so needed a CC2-capable host in beforeAll).
 *
 * OPERATOR-GATED: /policies' "New policy" button is disabled for a non-operator
 * (policies.tsx). The series shoots in LOCAL MODE (every session is
 * local:operator), so no sign-in beat is needed — but Act 1 asserts the button
 * is ENABLED before clicking, so a take accidentally shot against an SSO stack
 * fails loudly here instead of parking act() on a dead control for 45s.
 *
 * This is NOT a test. It asserts only enough to keep itself honest and to know
 * when to advance; a failure here means the recording is wrong, not that the
 * product is broken. It runs against the REAL compose stack on :8080, and the
 * meter needs the grade endpoint live (POST /api/v1/policies/grade, routes.go).
 *
 * Driven by `scripts/record-demo.sh --video 05`, which globs this exact
 * filename and names the take wardyn-05-your-first-policy-<stamp>.mp4 — so this
 * FILENAME IS LOAD-BEARING. It self-skips without WARDYN_DEMO=1 so a bare
 * `pnpm e2e` can never point a browser at a developer's live stack and start
 * deleting policies.
 *
 * The demo project records HEADLESS (playwright.config.ts): the browser records
 * ITSELF, there is no OS window during a take, and nothing here may depend on
 * window geometry. Every locator is a role, a label or page text.
 *
 * Every literal below exists in ui/src today (re-read 2026-08-24 against
 * policies.tsx, policy-panel.tsx, safety-meter.tsx and new-run/new-run-screen.tsx).
 *
 * DIALOG FIDELITY: every caption()/act() narration string here is a stanza line
 * in local/episode-05-policy-proposal.md (take order); local/episode-05-stanza-
 * check.py holds them in lockstep.
 */

import { test, expect, type Page } from "@playwright/test";
import { act, beat, caption, centerInFrame, chapter, PACE, spotlight } from "./overlay";
// stage.ts is the rig: importing it registers this file's beforeAll/afterAll
// (one browser, one context, one recorded page), and every beat reads the page
// out of stage() inside a test body rather than closing over a module binding.
import { stage } from "./stage";

test.skip(!process.env.WARDYN_DEMO, "demo recording — run via `make record-demo` (exports WARDYN_DEMO=1)");

test.describe.configure({ mode: "serial" });

// ---------------------------------------------------------------------------
// This video's nouns. Deliberately LOCAL, not in task.ts: this is the only
// episode that AUTHORS a policy, and a shared constant is how two videos end up
// fighting over one control-plane row.
// ---------------------------------------------------------------------------

/**
 * The policy this video authors. The name is memorable ON PURPOSE: new-06
 * ("Your first run") reuses it BY REFERENCE, so its lane must use this exact
 * string. Deleted per take (beforeAll) so the episode is immediately re-takeable.
 */
const POLICY_NAME = "first-policy";

/**
 * The saved spec — floored at CC2 (Wall) so Act 4's Barrier Seg has a floor to
 * refuse Fence beneath, and two allowed hosts so the panel's derived egress
 * chip reads "2 domains allowed" (egressSummary, policy-panel.tsx). deny_with_
 * review is the "everything else requires a decision" mode; auto_stop keeps the
 * saved policy off the meter's never-reap high.
 */
const POLICY_SPEC = {
  allowed_domains: ["github.com", "npmjs.org"],
  first_use_approval: "deny_with_review",
  min_confinement_class: "CC2",
  auto_stop_after_sec: 3600,
  eligible_grants: [],
} as const;

/** ConfinementChip's label for CC2 (cc-meta.ts) and egressSummary's phrasing. */
const POLICY_FLOOR_LABEL = "Wall";
const POLICY_EGRESS_SUMMARY = "2 domains allowed";

/** The owner's staccato lines read fast; PACE.read after one is dead air. */
const BEAT_SHORT = 1400;

/** Filled in by A4, read nowhere else (this episode does not launch). */
let createdPolicyId = "";

function apiHeaders(): Record<string, string> | undefined {
  // Local mode (the series' posture) has no auth; the token lane exists for a
  // stack booted with one. Same shape old-08's beforeAll used.
  return process.env.WARDYN_DEMO_TOKEN ? { Authorization: `Bearer ${process.env.WARDYN_DEMO_TOKEN}` } : undefined;
}

/** The editor Dialog, scoped by its title (the detail Sheet is role=dialog too). */
function editorDialog(page: Page) {
  return page.getByRole("dialog").filter({ hasText: "New policy" });
}

// ---------------------------------------------------------------------------
// Staging — the part that must not be prose.
//
// OFF-CAMERA PRECONDITIONS THE OPERATOR STILL OWNS (nothing below can encode
// these; check them before the take rolls):
//   1. The compose stack is up on :8080 and its grade endpoint answers (the
//      meter is dead without it).
//   2. The stack has NOT been reset since the earlier videos (record-demo.sh
//      --video 05 defaults DO_RESET=0; never pass --reset here).
//   3. The session is an OPERATOR. Local mode is, automatically; an SSO stack
//      shot as a member fails at Act 1's enabled-button assertion.
//   4. No model, no workspace needed. This video is keyless and launches nothing.
//
// Delete any policy already named POLICY_NAME, off camera, before the first
// beat: a stale row makes the create's getByRole("row", …) a strict-mode
// violation (two matches) that fails confusingly halfway through instead of
// here, and is what makes this video byte-identically re-takeable.
//
// Registered AFTER stage.ts's own beforeAll (import order), so stage() is
// assigned by the time this runs. Re-guards on WARDYN_DEMO because this hook
// DELETES a control-plane row: a file-level test.skip must never be the only
// thing standing between a developer's stack and that.
// ---------------------------------------------------------------------------
test.beforeAll(async () => {
  if (!process.env.WARDYN_DEMO) return;
  const page = stage();
  const headers = apiHeaders();

  const res = await page.request.get("/api/v1/policies", { headers });
  expect(res.ok(), `GET /api/v1/policies failed (${res.status()}) — is the stack up on :8080?`).toBe(true);
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
// A1 — the panel and its templates
// ---------------------------------------------------------------------------

test("A1 — the panel and its templates", async () => {
  test.setTimeout(180_000);
  const page = stage();
  await page.goto("/");
  await page.bringToFront();

  // App-shell mounted marker: the top bar's own New-run button is present on
  // every authenticated route regardless of which page "/" lands on.
  await expect(page.locator("header").getByRole("button", { name: "New run" })).toBeVisible({ timeout: 60_000 });

  // Fail here rather than at the end of a silent, caption-less take: every
  // narration call degrades to a no-op by design (overlay.ts's call()).
  await expect
    .poll(() => page.evaluate(() => typeof (window as unknown as Record<string, unknown>).__demo), { timeout: 15_000 })
    .toBe("object");

  await chapter(page, "Your first policy", "Write the rules once — every run can reuse them");

  await caption(page, "Before we launch our first run, let's write down the rules it will follow.");
  await beat(page, PACE.read);

  await act(page, page.getByRole("link", { name: "Policies" }), "Open Policies.");
  await expect(page.getByRole("heading", { name: "Policies", level: 1 })).toBeVisible({ timeout: 30_000 });

  // The operator gate, named. A member's button is disabled (policies.tsx) and
  // act() would simply park on it for the full 45s action timeout.
  const newPolicyBtn = page.getByRole("button", { name: "New policy" });
  await expect(
    newPolicyBtn,
    "the New policy button is disabled — this session is not an operator (shoot in local mode)",
  ).toBeEnabled({ timeout: 30_000 });

  await act(page, newPolicyBtn, "New policy.");
  const dlg = editorDialog(page);
  await expect(dlg.getByRole("heading", { name: "New policy" })).toBeVisible();

  // The modal's own body text is the most interesting thing on screen — ring
  // the "admin-gated config" description while the two intro lines land.
  await spotlight(page, dlg.getByText(/admin-gated config/));
  await caption(page, "A policy is a spec — a run's rules, saved once with a name so runs can reuse it.");
  await beat(page, PACE.read);
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
  // A row of chips swaps in the worked specs (policy-panel.tsx's
  // POLICY_TEMPLATES, seeded from examples/policies/*.json). The five-template
  // line names all five, so ASSERT all five before speaking it — a template
  // dropped from the product must not be narrated onto a screen that lost it.
  const templateRow = dlg.getByText("Start from a template");
  await spotlight(page, templateRow);
  await caption(page, "Start from a template.");
  await beat(page, BEAT_SHORT);
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

  // Click one that VISIBLY rewrites the document (Package registries = eleven
  // hosts) — the dialog prefills with Minimal, so a Minimal click here would be
  // a dead one. The readable JSON is the point of this beat.
  await act(page, dlg.getByRole("button", { name: "Package registries" }));

  // The helper rail documents every key with its legal values (FIELD_HELP). The
  // docs-reference footer is a unique anchor to ring.
  const railFooter = dlg.getByText("Full reference: docs/POLICIES.md");
  await centerInFrame(railFooter);
  await spotlight(page, railFooter);
  await caption(page, "And every field is documented right here — what it does, and the values it takes.");
  await beat(page, PACE.read);
  await spotlight(page, null);
});

// ---------------------------------------------------------------------------
// A2 — the safety meter (the star)
// ---------------------------------------------------------------------------

test("A2 — the safety meter", async () => {
  test.setTimeout(180_000);
  const page = stage();
  const dlg = editorDialog(page);
  const meter = dlg.getByTestId("safety-meter");
  const specBox = dlg.getByLabel("Spec (JSON)");

  // The meter is already grading Act 1's Package-registries document. Introduce
  // it, then surface its own title — it grades the DOCUMENT, not any one run.
  await centerInFrame(meter);
  await spotlight(page, meter);
  await caption(page, "Every policy you write gets a safety reading, live, as you type.");
  await beat(page, PACE.read);
  await expect(
    meter,
    "the meter's title no longer says it grades the document — the document-vs-preflight distinction is gone",
  ).toHaveAttribute("title", /Safety of the policy document as written/);
  await caption(page, "It grades the document as written — not any single run that might use it.");
  await beat(page, PACE.read);
  await caption(page, "Safest on the left, weakest on the right.");
  await beat(page, PACE.read);

  // ── THE GRADE-WALKED RAMP ─────────────────────────────────────────────
  // Every label asserted via data-safety (grade is async + debounced;
  // toHaveAttribute auto-retries until it lands). VERIFY the captions against
  // the live meter in rehearsal.

  // Minimal template -> Guarded (CC2 = one medium; small egress + deny_with_
  // review + auto_stop = no higher item).
  await act(page, dlg.getByRole("button", { name: "Minimal" }));
  await expect(meter, "Minimal did not read Guarded — the CC2 medium is not the overall level").toHaveAttribute(
    "data-safety",
    "Guarded",
  );
  await spotlight(page, meter);
  await caption(page, "Our minimal starter reads guarded.");
  await beat(page, PACE.read);

  // Hand-edit the floor to CC3 -> Safest (all low). Read-and-replace the one
  // field, exactly like a real edit; fill() replaces the textarea in one call.
  await specBox.fill((await specBox.inputValue()).replace('"CC2"', '"CC3"'));
  await expect(meter, "raising the floor to CC3 did not read Safest").toHaveAttribute("data-safety", "Safest");
  await spotlight(page, meter);
  await caption(page, "Raise the barrier to its strongest, and there's nothing here to reach out or write with — safest.");
  await beat(page, PACE.read);

  // Drop the floor to CC1 -> Elevated (CC1 = exactly one high item).
  await specBox.fill((await specBox.inputValue()).replace('"CC3"', '"CC1"'));
  await expect(meter, "dropping the floor to CC1 did not read Elevated").toHaveAttribute("data-safety", "Elevated");
  await spotlight(page, meter);
  await caption(page, "Drop to the weakest barrier, and the reading climbs to elevated.");
  await beat(page, PACE.read);

  // Allow-all template -> Weakest (allow-all egress + never-reap = two highs).
  // A single field edit can only add ONE high, so the second high has to come
  // from the template, not another CC edit.
  await act(page, dlg.getByRole("button", { name: "Allow-all — observe first" }));
  await expect(meter, "the allow-all template did not read Weakest — two highs are not both present").toHaveAttribute(
    "data-safety",
    "Weakest",
  );
  await spotlight(page, meter);
  await caption(page, "Open egress to the whole internet, and it reads weakest — the most a policy can hand away.");
  await beat(page, PACE.read);

  await caption(page, "The meter never blocks a save — it just shows you, up front, how much room you're giving a run.");
  await beat(page, PACE.read + 400);
  await spotlight(page, null);
});

// ---------------------------------------------------------------------------
// A3 — the confinement floor
// ---------------------------------------------------------------------------

test("A3 — the confinement floor", async () => {
  test.setTimeout(180_000);
  const page = stage();
  const dlg = editorDialog(page);
  const specBox = dlg.getByLabel("Spec (JSON)");

  // Settle on the policy we will save — floored at CC2, two hosts, review-first.
  await specBox.fill(JSON.stringify(POLICY_SPEC, null, 2));

  // The panel's LIVE derivations, re-read on every keystroke. The egress chip
  // proves the fill landed (Allow-all a moment ago showed the block-list
  // phrasing; two hosts read "2 domains allowed").
  const egressChip = dlg.getByText(POLICY_EGRESS_SUMMARY, { exact: true });
  await expect(egressChip, "the panel does not read '2 domains allowed' — the fill never replaced the allow-all spec").toBeVisible();
  await spotlight(page, egressChip);
  await caption(page, "We'll give this one two allowed hosts.");
  await beat(page, PACE.read);
  await spotlight(page, specBox);
  await caption(page, "Everything else should require a decision.");
  await beat(page, PACE.read);

  // The helper rail documents every key; each row is located by its own
  // "Insert <key>" button (a unique aria-label) rather than the key text, which
  // also appears inside the textarea above.
  const railEntry = (key: string) => dlg.getByRole("button", { name: `Insert ${key}` }).locator("xpath=ancestor::li[1]");
  const firstUse = railEntry("first_use_approval");
  await centerInFrame(firstUse);
  await spotlight(page, firstUse);
  await caption(page, "This setting controls that behavior.");
  await beat(page, PACE.read);
  const floorField = railEntry("min_confinement_class");
  await centerInFrame(floorField);
  await spotlight(page, floorField);
  await caption(page, "And this one sets the minimum confinement level.");
  await beat(page, PACE.read);

  // The floor BADGE — the panel's own derived chip (ConfinementChip "Wall"),
  // not the raw JSON.
  await spotlight(page, dlg.getByText(POLICY_FLOOR_LABEL, { exact: true }).first());
  await caption(page, "Think of it as the floor.");
  await beat(page, BEAT_SHORT);
  await caption(page, "The policy can require a stronger barrier than the machine's default.");
  await beat(page, PACE.read);
  // The RULE, always true (a floor above what a host can build refuses to
  // start there — resolveEnforcedConfinement, runs.go), so nothing here names a
  // tier the host might contradict.
  await caption(page, "But it can't ask a machine to provide a barrier it doesn't support.");
  await beat(page, PACE.read);

  // The barrier-tier callback (old-08 B1, lifted). Episode 02 already walks
  // Fence/Wall/Vault in full, so this is the callback plus the one thing 02
  // could not say — here the pick is saved WITH the policy.
  await spotlight(page, floorField);
  await caption(page, "You've already met Fence, Wall, and Vault, and the same capability check applies here.");
  await beat(page, PACE.read);
  await caption(
    page,
    "The difference is that this time the choice gets saved with the policy, instead of being made again every run.",
  );
  await beat(page, PACE.read + 400);
  await spotlight(page, null);
});

// ---------------------------------------------------------------------------
// A4 — save, then reuse
// ---------------------------------------------------------------------------

test("A4 — save, then reuse", async () => {
  test.setTimeout(180_000);
  const page = stage();
  const dlg = editorDialog(page);

  await act(page, dlg.getByRole("button", { name: "Create policy" }), "Create.");
  await expect(dlg).toBeHidden({ timeout: 30_000 });

  const row = page.getByRole("row", { name: new RegExp(POLICY_NAME) });
  await expect(row).toBeVisible({ timeout: 30_000 });
  // The saved row matches what we authored: the Wall floor and the two-host
  // egress (a fill that silently failed would read the allow-all block-list here).
  await expect(row.getByText(POLICY_FLOOR_LABEL, { exact: true }), "the saved policy's floor is not Wall").toBeVisible();
  await expect(
    row.getByText(POLICY_EGRESS_SUMMARY, { exact: true }),
    "not '2 domains allowed' — the saved egress does not match what we authored",
  ).toBeVisible();

  // ID column: Name(0), ID(1), … — policies.tsx's own column order.
  createdPolicyId = (await row.getByRole("cell").nth(1).textContent())?.trim() ?? "";
  expect(createdPolicyId, "policy row rendered but its ID cell read empty").not.toBe("");

  await spotlight(page, row);
  await caption(page, "Saved.");
  await beat(page, BEAT_SHORT);
  await caption(page, "And now this policy has an identity.");
  await beat(page, PACE.read);
  await caption(page, "Every run that uses it can point back to the exact rules that governed it.");
  await beat(page, PACE.read + 400);
  await spotlight(page, null);

  // ── REUSE, BY REFERENCE, ON THE RUN FORM ──────────────────────────────
  // The setup for episode 06: a run just points at the saved policy. This
  // episode STOPS before launch.
  await act(page, page.locator("header").getByRole("button", { name: "New run" }), "New run.");
  await expect(page).toHaveURL(/\/runs\/new/, { timeout: 30_000 });

  // "Reuse a saved policy" is the Policy panel's MODE ROW (an OptionCard — an
  // aria-pressed <button> whose accessible name is its title plus its hint, so
  // prefix-match the title). The picker renders once this half is lit.
  await act(page, page.getByRole("button", { name: /^Reuse a saved policy/ }), "Select saved policy.");
  await act(page, page.getByRole("combobox", { name: "Saved policy" }));
  await act(page, page.getByRole("option", { name: POLICY_NAME }));

  // The rail names the STORED policy, its own floor and host count, and says
  // outright that nothing on this page is merged into it. Scoped to the rail
  // (the sidebar is an <aside>) because the name also shows in the Select trigger.
  const rail = page.locator("aside").filter({ hasText: "What this run can do" });
  await expect(rail.getByText(POLICY_NAME, { exact: true })).toBeVisible();
  const railPolicy = rail.getByText(/^The stored spec governs this run/);
  await expect(railPolicy).toBeVisible();

  await spotlight(page, rail);
  await caption(page, "There it is — the policy we just saved, ready to reuse by name.");
  await beat(page, PACE.read);
  await caption(page, "A run doesn't copy these rules. It points at them.");
  await beat(page, PACE.read);
  await caption(page, "And here's the important distinction.");
  await beat(page, PACE.read);

  // Barrier = the wall (the run's requested class, a separate wire field from
  // the policy floor); policy = the rules the rail is showing.
  const barrier = page.getByRole("radiogroup", { name: "Barrier" });
  await spotlight(page, barrier);
  await caption(page, "The sandbox is the wall around the workload.");
  await beat(page, PACE.read);
  await spotlight(page, railPolicy);
  await caption(page, "The policy is the set of rules governing what happens around that wall.");
  await beat(page, PACE.read);
  await caption(page, "The policy brings those rules with it.");
  await beat(page, PACE.read + 400);
  await spotlight(page, null);

  // ── THE FLOOR, FILMED ─────────────────────────────────────────────────
  // A STANDING refusal: the Barrier Seg disables every tier below the attached
  // policy's floor and prints its own reason line, so Fence is greyed the moment
  // the policy is picked. RING it rather than click it — clicking a disabled
  // button would park act() for the full 45s action timeout. (Lifted from
  // old-08 B4; three lines carry old-08's DIALOG-STALE note.)
  const fenceRadio = barrier.getByRole("radio", { name: "Fence" });
  await centerInFrame(fenceRadio);
  await spotlight(page, fenceRadio);
  await caption(page, "Now we'll deliberately choose a barrier below the policy's minimum.");
  await beat(page, PACE.read);
  await expect(
    fenceRadio,
    "Fence is selectable beneath the policy's CC2 floor — the floor is not being enforced on the form",
  ).toBeDisabled();

  // The Seg's OWN reason line for a floor-disabled tier — distinct from the
  // "isn't installed on this host" line. Both labels are OUR policy's floor and
  // OUR choice of tier, never a claim about the host.
  const floorReason = page.getByText(`Fence is below the policy's floor (${POLICY_FLOOR_LABEL}).`);
  await expect(floorReason, "the Seg disables Fence without saying why — the floor is enforced but not explained").toBeVisible();
  await spotlight(page, floorReason);
  await caption(page, "And Wardyn refuses to start it.");
  await beat(page, PACE.read);
  await caption(page, "The policy requires a stronger floor.");
  await beat(page, PACE.read);
  await caption(page, "So the control plane says no.");
  await beat(page, PACE.read + 400);
  await spotlight(page, null);

  // ---- Conclusion — points at 06, which launches this very policy -----------
  await caption(page, "So that's our first policy — written once, and saved for good.");
  await beat(page, PACE.read);
  await caption(page, "In the next episode, we launch a run that uses it.");
  await beat(page, PACE.read + 400);
  await silentCard(page, "Next — 06: Your first run");
});

/**
 * A chapter card that is NOT spoken — the outro card convention episode 01 set.
 * overlay.ts's chapter() always speaks what it renders; this drives the same
 * overlay primitive directly for the one card that must stay silent.
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
