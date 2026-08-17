/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * V08 — Policies & confinement tiers.
 *
 * Films the honesty of the barrier picker (Settings' Host card names exactly
 * which of Fence/Wall/Vault THIS host can build, never a barrier it cannot
 * enforce), then the reusable half of governance: write a policy once, and a
 * later run references it by id instead of re-declaring its whole envelope on
 * the New Run form. Four beats, ~1:48 of narration
 * (docs/DEMO-SCRIPT.md / the campaign plan's V06 section is the source of
 * truth for every SAY line below — copied verbatim, never reworded).
 *
 * KEYLESS, by explicit instruction for this take (superseding the plan
 * section's own STAGING note "model pre-connected (no rail warning)" — that
 * text predates the decision to shoot V06 with no model connected at all).
 * B3 picks "Shell command" as the run type specifically so nothing on this
 * page ever asks about a model: no Agent select, no Credentials rail warning,
 * no dependency on a subscription/API key being wired up off camera. That is
 * the one SCREEN action the plan's own B3 line doesn't spell out; it plays
 * silent, the same convention SV15 uses below for the cut "Launch." caption.
 *
 * OPERATOR-GATED: /policies' "New policy" button is disabled for a non-
 * operator (policies.tsx). This needs no separate sign-in beat because the
 * whole demo series shoots in LOCAL MODE (SV1, walkthrough.spec.ts's own
 * series-wide posture) — every session is stamped `local:operator`
 * automatically, the same "no account, no cloud sign-in" promise V01 opens
 * on. B2 still asserts the button is enabled before clicking it, so a take
 * accidentally shot against a non-local (SSO) stack fails loudly here instead
 * of the click silently no-op'ing on a disabled button.
 *
 * FRICTION NOTES VERIFIED AGAINST CURRENT SOURCE (2026-08-17) — three of the
 * plan's four Track-A findings for this video are ALREADY FIXED on this
 * branch; only the STARTER_SPEC trap is still live:
 *   (1) FIXED — new-run-screen.tsx:357-361 now gates the launch payload on
 *       the confinement RADIO, not a stale selectedPolicyId alone (the
 *       comment there names the exact bug this closes).
 *   (2) FIXED — the rail's "Policy" section (new-run-screen.tsx:700-712) now
 *       names the stored policy's own barrier + host count when Saved policy
 *       is picked, and hides the wizard's own Network rail section under it
 *       ("The network edits on this page do not apply to it."). B3 below
 *       spotlights it — the plan's own note that "rail-reflects-policy would
 *       add one line" is now moot; no new caption text was added, just a
 *       camera direction, since it already exists on screen.
 *   (3) STILL LIVE — policies.tsx's STARTER_SPEC floors at CC2; a policy
 *       created by editing it in place 422s at Launch on this Fence-only
 *       (CC1) host with no editor-side warning. B2 never edits it — it PASTES
 *       a prepared CC1 spec over the whole textarea via .fill(), which
 *       replaces the field's value in one call exactly like a real paste,
 *       never keystroking over the CC2 starter.
 *   (4) FIXED — new-run-screen.tsx:162-165's resolveDefaultCc no longer
 *       hardcodes ["CC1"] as the available-classes list, so Settings'
 *       "saved as your default barrier" promise now holds on New Run too
 *       (moot for this take either way: this host has only CC1 available, so
 *       the default resolves to Fence regardless of what's persisted).
 *
 * Runs after V01's reset and the V08/V02/V03 quota window (Phase 4 order) —
 * a shared, already-set-up local-mode stack. This file makes no assumption
 * about which page "/" lands on: it waits for the app shell itself (the top
 * bar's own New-run button) rather than a specific route.
 */

import { test, expect, type Page } from "@playwright/test";
import { act, beat, caption, chapter, PACE, spotlight } from "./overlay";
import { stage } from "./stage";

test.skip(!process.env.WARDYN_DEMO, "demo recording — run via `make record-demo` (exports WARDYN_DEMO=1)");

test.describe.configure({ mode: "serial" });

// The policy this video creates and then launches under. Four fields, matching
// the SAY line "The spec is four fields." exactly — and min_confinement_class
// is CC1 on purpose (see the STARTER_SPEC trap note above): this is the
// prepared string B2 pastes, never the editor's own CC2-floored starter.
const POLICY_NAME = "nightly-triage";
const POLICY_SPEC = {
  allowed_domains: ["github.com", "npmjs.org"],
  first_use_approval: "wait_for_review",
  min_confinement_class: "CC1",
  eligible_grants: [],
} as const;

// B3's run. A Shell command, not an Agent task — see the file header's
// KEYLESS note. The command itself does nothing that could fail: this video
// is not about what the run DOES, only about which policy governed it.
const DEMO_RUN_TITLE = "Nightly triage, governed by policy";
const DEMO_COMMAND = 'echo "nightly triage: running under the saved policy"';

// Filled in by B2, read by B4 — proves B4's Identity-widget assertion checks
// the SAVED policy's own id, not merely "a" policy id that happens to exist.
let createdPolicyId = "";

/**
 * Delete any policy already named POLICY_NAME, via the API.
 *
 * Mirrors funnel.ts's clearWorkspace(): a stale "nightly-triage" row left
 * over from a prior --no-reset take makes every getByRole("row", …) query in
 * B2 a strict-mode violation (two matches), not a wrong-row bug — so this
 * runs first, off camera, rather than leaving the take to fail confusingly
 * partway through.
 */
async function clearPolicy(page: Page): Promise<void> {
  const res = await page.request.get("/api/v1/policies").catch(() => null);
  if (!res?.ok()) return;
  const body = await res.json().catch(() => null);
  const items: { id?: string; name?: string }[] = Array.isArray(body) ? body : [];
  for (const p of items) {
    if (p?.id && p.name === POLICY_NAME) {
      await page.request.delete(`/api/v1/policies/${p.id}`).catch(() => {});
    }
  }
}

// ---------------------------------------------------------------------------
// B1 — barrier honesty
// ---------------------------------------------------------------------------

test("B1 — barrier honesty", async () => {
  test.setTimeout(120_000);
  const page = stage();
  await page.goto("/");
  await page.bringToFront();

  // App-shell mounted marker: the top bar's own New-run button is present on
  // every authenticated route, regardless of which page "/" happens to land
  // on for an already-set-up stack (unlike V01, this video does not care).
  await expect(page.locator("header").getByRole("button", { name: "New run" })).toBeVisible({ timeout: 60_000 });

  // Fail here rather than at the end of a silent, caption-less take: every
  // narration call degrades to a no-op by design (overlay.ts's call()), so
  // nothing downstream ever complains about an overlay that failed to install.
  // Same preflight the other governance videos carry.
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

  // Silent nav — the SAY lines below narrate what's now on screen, not the
  // click that got here.
  await act(page, page.getByRole("link", { name: /Sandbox barrier — open Settings/ }));
  await expect(page.getByRole("heading", { name: "Host", level: 3 })).toBeVisible({ timeout: 30_000 });

  const matrix = page.getByRole("radiogroup", { name: "Barrier tier" });
  await expect(matrix).toBeVisible({ timeout: 30_000 });

  await caption(page, "This card names the barriers this machine can actually build.");
  await spotlight(page, page.getByRole("heading", { name: "Host", level: 3 }));
  await beat(page, PACE.read);
  await spotlight(page, null);

  await caption(page, "Fence, ready. Wall, needs setup. Vault, incompatible here.");
  // ASSERT THE PAYOFF: this line claims a specific three-way verdict for this
  // exact host. If this stack ever stops being Fence-only (Wall gets set up,
  // Vault gets KVM), the claim goes false and the take must fail loudly here
  // rather than record over a screen that no longer agrees with the script.
  const readyChip = matrix.getByText("Ready", { exact: true });
  const needsSetupChip = matrix.getByText("Needs setup", { exact: true });
  const incompatibleChip = matrix.getByText("Incompatible here", { exact: true });
  await expect(readyChip).toBeVisible();
  await expect(needsSetupChip).toBeVisible();
  await expect(incompatibleChip).toBeVisible();
  await spotlight(page, readyChip);
  await beat(page, 700);
  await spotlight(page, needsSetupChip);
  await beat(page, 700);
  await spotlight(page, incompatibleChip);
  await beat(page, PACE.read);
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
  test.setTimeout(120_000);
  const page = stage();

  await clearPolicy(page);

  await act(page, page.getByRole("link", { name: "Policies" }));
  await expect(page.getByRole("heading", { name: "Policies", level: 1 })).toBeVisible({ timeout: 30_000 });

  const newPolicyBtn = page.getByRole("button", { name: "New policy" });
  await expect(newPolicyBtn).toBeEnabled({ timeout: 30_000 });

  await act(page, newPolicyBtn);
  const dlg = page.getByRole("dialog");
  await expect(dlg.getByRole("heading", { name: "New policy" })).toBeVisible();

  await caption(page, "Name it. The spec is four fields.");
  const nameBox = dlg.getByLabel("Name");
  await spotlight(page, nameBox);
  await nameBox.fill(POLICY_NAME);
  await spotlight(page, null);
  await beat(page, PACE.read);

  // PASTE, never edit the CC2-floored STARTER_SPEC in place — see the file
  // header's friction note (3). fill() replaces the textarea's whole value in
  // one call, functionally identical to a real clipboard paste; it never
  // keystrokes over the prefilled starter.
  const specBox = dlg.getByLabel("Spec (JSON)");
  await spotlight(page, specBox);
  await specBox.fill(JSON.stringify(POLICY_SPEC, null, 2));
  await beat(page, PACE.read);

  await caption(page, "Two hosts allowed. Anything unlisted is held for your approval.");
  await beat(page, PACE.read);
  await caption(page, "Min confinement class is the floor — the weakest barrier this policy accepts.");
  await beat(page, PACE.read);
  await caption(page, "Set it to Wall, and this run refuses to start here.");
  await beat(page, PACE.read + 500);
  await spotlight(page, null);

  await act(page, dlg.getByRole("button", { name: "Create policy" }));
  await expect(dlg).toBeHidden({ timeout: 30_000 });

  const row = page.getByRole("row", { name: new RegExp(POLICY_NAME) });
  await expect(row).toBeVisible({ timeout: 30_000 });
  // ID column: Name(0), ID(1), Barrier(2), Egress(3), Attributes(4), Updated(5)
  // — policies.tsx's own column order.
  createdPolicyId = (await row.getByRole("cell").nth(1).textContent())?.trim() ?? "";
  expect(createdPolicyId, "policy row rendered but its ID cell read empty").not.toBe("");
});

// ---------------------------------------------------------------------------
// B3 — use it
// ---------------------------------------------------------------------------

test("B3 — use it", async () => {
  test.setTimeout(120_000);
  const page = stage();

  await act(page, page.locator("header").getByRole("button", { name: "New run" }));
  await expect(page).toHaveURL(/\/runs\/new/, { timeout: 30_000 });

  const titleBox = page.getByLabel("Title");
  await spotlight(page, titleBox);
  await titleBox.fill(DEMO_RUN_TITLE);
  await spotlight(page, null);

  // KEYLESS (file header): Shell command needs no agent, no model, no
  // Credentials-rail warning — silent, the one SCREEN action the plan's own
  // B3 line doesn't narrate.
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
  // Friction (2), now fixed: the rail names the stored policy outright
  // instead of describing wizard state a saved-policy launch would drop.
  // .last() — the same name is also still showing in the Select trigger to
  // its left; the rail's own paragraph mounts after it in DOM order.
  const railPolicyName = page.getByText(POLICY_NAME, { exact: true }).last();
  await expect(railPolicyName).toBeVisible();
  await spotlight(page, railPolicyName);
  await beat(page, PACE.read);
  await spotlight(page, null);
});

// ---------------------------------------------------------------------------
// B4 — launch, effective policy
// ---------------------------------------------------------------------------

test("B4 — launch, effective policy", async () => {
  test.setTimeout(180_000);
  const page = stage();

  // SV15: the click plays silent — "Launch." was cut from the script; the
  // next line carries the beat instead.
  await act(page, page.getByRole("button", { name: "Launch run" }));
  await expect(page).toHaveURL(/\/runs\/[0-9a-f-]{8,}/i, { timeout: 60_000 });

  // #main-content excludes the app-shell's OWN top-bar barrier chip, which
  // also reads "Fence" on every route — without this scope the run header's
  // chip is not the only match.
  const main = page.locator("#main-content");
  const headerFence = main.getByText("Fence", { exact: true }).first();
  await expect(headerFence).toBeVisible({ timeout: 30_000 });
  await caption(page, "Fence in the header. The policy it ran under, on Identity.");
  await spotlight(page, headerFence);
  await beat(page, PACE.read);
  await spotlight(page, null);

  // ASSERT THE PAYOFF: the Identity widget must show THIS SAVED policy's own
  // id, not some inline or default policy that only coincidentally also runs
  // at Fence — that is the entire claim "not what you typed" is making on
  // camera. spotlight() scrolls it into view on its own; the widget sits
  // below the live rail's fold at 1080p (V03's own friction note).
  const policyRow = main.getByText(createdPolicyId, { exact: true });
  await expect(policyRow).toBeVisible({ timeout: 30_000 });
  await caption(page, "Not what you typed. What the control plane enforced.");
  await spotlight(page, policyRow);
  await beat(page, PACE.read);
  await spotlight(page, null);

  await caption(page, "One policy. Every run after it, already governed.");
  await beat(page, PACE.read);
  await caption(page, "Next: the same guarantees with nobody watching — Wardyn inside a CI pipeline.");
  await beat(page, PACE.chapter);
  await caption(page, "");
});
