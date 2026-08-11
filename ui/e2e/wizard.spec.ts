/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New Run wizard (PermissionWizard) e2e — the 5-step manual flow:
//   Basics → Access → Egress → Confinement → Review.
// Source of truth read before writing selectors:
//   src/app/components/screens/new-run/{wizard,step-basics,step-access,
//     step-egress,step-confinement,step-review,step-shell,wizard-types}.tsx
//   src/app/components/wardyn/{cc-meta,copy,status-chip}.ts(x)
//
// Notes on the seeded backend (port from WARDYN_E2E_BASE_URL):
//   /healthz reports no confinement_classes, so the barrier picker floors to the
//   Fence (CC1): the Wall (gVisor) and Vault (Kata) cards render disabled with a
//   StatusChip "Unavailable here" + a concrete hardware reason. Wizard defaults:
//   claude-code agent, interactive mode (task optional), local workspace
//   (read-write, empty path), one preset allowed domain (api.anthropic.com),
//   Fence barrier. Model access is RESOLVED, not configured: the seeded backend
//   has no ai_provider integration at all (no anthropic-api-key secret, no
//   subscription), so Access's model-access card always shows the honest "no
//   integration can drive this agent" line, never an auth choice.
//
// The barrier tiers render as the USER labels Fence/Wall/Vault — the wire codes
// CC1/CC2/CC3 never leak as visible text on the picker (only in title tooltips),
// and the isolation SUBSTRATE (gVisor/runc/Kata) lives in tooltips too.
import { test, expect, gotoConsole } from "./fixtures";
import type { Page, Locator } from "@playwright/test";

// The wizard renders inside a Dialog; scope everything to it so step copy never
// collides with the screen behind it.
function wizard(page: Page): Locator {
  return page.getByRole("dialog");
}

// Open the shell top-bar "New run" dialog. It now opens on the workspace-first
// chooser (Stage 3) ahead of Describe/Configure; this helper clears that step
// via the "No workspace — ad-hoc run" escape so fillValidBasics' own
// combobox-attach below still exercises Basics' picker as a separate,
// still-editable step (see "picks the seeded workspace directly from the
// entry chooser" further down for the direct-seed path instead). With
// composer backends configured (scripts/e2e-backend.sh), the manual wizard is
// then reached via "Configure manually".
async function openWizard(page: Page): Promise<Locator> {
  await gotoConsole(page);
  const backends = page.waitForResponse((r) => /\/composer\/backends/.test(r.url()));
  await page.getByRole("button", { name: "New run" }).click();
  await backends;
  const dlg = wizard(page);
  await expect(dlg.getByRole("heading", { name: "New run" })).toBeVisible();
  await dlg.getByRole("button", { name: /No workspace — ad-hoc run/ }).click();
  await dlg.getByRole("button", { name: /Configure manually/ }).click();
  // Step 1 is Basics — its onboarded-Workspaces field proves we're on it.
  await expect(dlg.getByText("Workspaces", { exact: true })).toBeVisible();
  return dlg;
}

// Fill the Basics step by attaching the seeded onboarded workspace ("payments",
// seeded by scripts/e2e-backend.sh) so Next un-gates. Raw host paths are gone —
// the mount gate accepts onboarded workspaces only.
async function fillValidBasics(dlg: Locator) {
  await dlg.getByRole("combobox", { name: /Add a workspace/ }).click();
  // The combobox list renders in a portal OUTSIDE the wizard dialog.
  await dlg.page().getByRole("option", { name: /payments/ }).click();
  await expect(dlg.getByText("primary", { exact: true })).toBeVisible();
  // The picker's other half of the contract: what the workspace carries into
  // every run, stated up front rather than re-asked. The seeded "payments"
  // workspace has no requirements contract (never scanned, never PUT), so it
  // reads the honest empty-contract fallback (wizard-types.ts's comesWithLine).
  await expect(dlg.getByText("Comes with:")).toBeVisible();
  await expect(dlg.getByText(/nothing beyond the auto-allowed set/i)).toBeVisible();
}

async function next(dlg: Locator) {
  await dlg.getByRole("button", { name: "Next" }).click();
}

// N1: the seeded backend's `-runner none` reports NO confinement classes at
// all (see the file header above), so EVERY wizard run below floors to
// Fence/CC1 — which grades HIGH unconditionally (risk.go:108-110), regardless
// of anything else the operator picked. Every Review reached through the
// default flow in this file therefore carries a HIGH item and needs this SAME
// acknowledgment before Launch un-gates — the identical RiskPanel gate the AI
// Run Composer's Review already enforces (compose-review.tsx), now shared
// verbatim by the manual wizard's Review (step-review.tsx). Locator .click()
// auto-waits, so callers don't need their own wait for preflight to resolve.
async function ackHighRisk(dlg: Locator): Promise<void> {
  await dlg.getByRole("checkbox").click();
}

test.describe("New Run wizard", () => {
  test("opens from the shell and shows the 5-step indicator", async ({ page }) => {
    const dlg = await openWizard(page);
    for (const label of ["Basics", "Access", "Egress", "Confinement", "Review"]) {
      await expect(dlg.getByRole("button", { name: label })).toBeVisible();
    }
    // Cancel closes the dialog without creating anything.
    await dlg.getByRole("button", { name: "Cancel" }).click();
    await expect(page.getByRole("dialog")).not.toBeVisible();
  });

  test("picks the seeded workspace directly from the entry chooser, and Basics arrives pre-seeded", async ({
    page,
  }) => {
    await gotoConsole(page);
    const backends = page.waitForResponse((r) => /\/composer\/backends/.test(r.url()));
    await page.getByRole("button", { name: "New run" }).click();
    await backends;
    const dlg = wizard(page);

    // The entry chooser shows the seeded "payments" workspace as a card
    // (name + status + source) alongside the ad-hoc escape, BEFORE either
    // Describe or Configure is offered.
    const card = dlg.getByRole("button", { name: /payments/ });
    await expect(card).toBeVisible();
    await expect(card.getByText("/home/me/projects/payments")).toBeVisible();
    await expect(dlg.getByRole("button", { name: /No workspace — ad-hoc run/ })).toBeVisible();
    await expect(dlg.getByRole("button", { name: /Describe your task/ })).toHaveCount(0);

    await card.click();
    await dlg.getByRole("button", { name: /Configure manually/ }).click();

    // Basics already shows it selected as the primary — no combobox needed.
    await expect(dlg.getByText("Workspaces", { exact: true })).toBeVisible();
    await expect(dlg.getByText("payments", { exact: true })).toBeVisible();
    await expect(dlg.getByText("primary", { exact: true })).toBeVisible();
  });

  test("the entry chooser's Back returns from Describe/Configure to the workspace pick", async ({
    page,
  }) => {
    await gotoConsole(page);
    const backends = page.waitForResponse((r) => /\/composer\/backends/.test(r.url()));
    await page.getByRole("button", { name: "New run" }).click();
    await backends;
    const dlg = wizard(page);

    await dlg.getByRole("button", { name: /No workspace — ad-hoc run/ }).click();
    await expect(dlg.getByRole("button", { name: /Configure manually/ })).toBeVisible();

    await dlg.getByRole("button", { name: "Back" }).click();
    await expect(dlg.getByRole("button", { name: /No workspace — ad-hoc run/ })).toBeVisible();
  });

  test("ephemeral runs need no workspace: Next is enabled and Review shows no repo", async ({ page }) => {
    const dlg = await openWizard(page);
    const nextBtn = dlg.getByRole("button", { name: "Next" });
    // No workspace attached — an interactive ephemeral scratch run is valid, so
    // Next un-gates immediately (the workspace requirement is gone).
    await expect(nextBtn).toBeEnabled();
    await next(dlg); // → Access
    await next(dlg); // → Egress
    await next(dlg); // → Barrier
    await next(dlg); // → Review
    // With no workspace the Review says so in words rather than leaving an
    // em-dash to be read: step-review.tsx renders
    // `<Summary label="Workspace" value="none (ephemeral scratch)" />`. The old
    // "Repo" summary is gone — it was the synthetic `local:<basename>` wire
    // label leaking into a field the operator never set.
    // exact:true — the attached-sources block below is labelled "Workspaces".
    await expect(dlg.getByText("Workspace", { exact: true })).toBeVisible();
    await expect(dlg.getByText("none (ephemeral scratch)")).toBeVisible();
  });

  test("autonomous mode requires a task before advancing, and says why", async ({ page }) => {
    const dlg = await openWizard(page);
    await fillValidBasics(dlg);
    // Switch to Autonomous — now the task is required (validateStep basics).
    await dlg.getByRole("radio", { name: "Autonomous" }).click();
    const nextBtn = dlg.getByRole("button", { name: "Next" });
    await expect(nextBtn).toBeDisabled();
    // N2: the block used to be a silent grey button — validateStep's message
    // is now rendered inline, not dead copy.
    await expect(dlg.getByText("An autonomous run needs a task to perform.")).toBeVisible();
    await dlg.getByPlaceholder("Describe what the agent should accomplish…").fill("Run the audit");
    await expect(nextBtn).toBeEnabled();
    await expect(dlg.getByText("An autonomous run needs a task to perform.")).toHaveCount(0);
  });

  test("steps through all five steps end to end", async ({ page }) => {
    const dlg = await openWizard(page);

    // Step 1 — Basics
    await fillValidBasics(dlg);
    await next(dlg);

    // Step 2 — Access: model access RESOLVES (no auth choice); defaults are
    // valid regardless (github disabled). The seeded backend has no
    // ai_provider integration, so the card shows the honest "none" line.
    await expect(dlg.getByText(/model access — resolved from integrations/i)).toBeVisible();
    // Generous timeout, deliberately: this line renders only after the card's
    // three self-fetches settle, and /setup/status (host/deployment probes) can
    // exceed the 5s default on a freshly-booted e2e backend still shouldering
    // the previous spec's teardown. The "Resolving model access…" interim state
    // is the honest loading gate, not a bug — the test just has to outwait it.
    await expect(dlg.getByText(/no integration can drive claude code/i)).toBeVisible({
      timeout: 15000,
    });
    await next(dlg);

    // Step 3 — Egress (default has api.anthropic.com preset selected → valid)
    await expect(dlg.getByText("Allowed domains")).toBeVisible();
    await next(dlg);

    // Step 4 — the Barrier picker. "Barrier" matches twice (the Field label +
    // the every-tier note's "Whatever the barrier…" — getByText is substring,
    // case-insensitive), so first() avoids strict mode.
    await expect(dlg.getByText("Barrier").first()).toBeVisible();
    await next(dlg);

    // Step 5 — Review (last step shows "Launch run", not "Next")
    await expect(dlg.getByText("inline_policy (sent verbatim)")).toBeVisible();
    await expect(dlg.getByRole("button", { name: "Launch run" })).toBeVisible();
    await expect(dlg.getByRole("button", { name: "Next" })).toHaveCount(0);
  });

  test("back-navigation preserves entered values", async ({ page }) => {
    const dlg = await openWizard(page);

    await fillValidBasics(dlg);
    await next(dlg);

    // On Access, enable the GitHub token grant and type repos.
    await expect(dlg.getByText(/model access — resolved from integrations/i)).toBeVisible();
    await dlg.getByLabel("GitHub token").click();
    const repos = dlg.getByPlaceholder("acme/payments-service, acme/shared-libs");
    await expect(repos).toBeVisible();
    await repos.fill("acme/preserved-repo");

    // Back to Basics — the attached workspace must still be there.
    await dlg.getByRole("button", { name: "Back" }).click();
    await expect(dlg.getByText("payments", { exact: true })).toBeVisible();
    await expect(dlg.getByText("primary", { exact: true })).toBeVisible();

    // Forward again — the repos value on Access must be preserved.
    await next(dlg);
    await expect(dlg.getByPlaceholder("acme/payments-service, acme/shared-libs")).toHaveValue(
      "acme/preserved-repo",
    );
  });

  test("the Barrier step is a metals picker with honest tier labels (no CCx / substrate leak)", async ({
    page,
  }) => {
    const dlg = await openWizard(page);
    await fillValidBasics(dlg);
    await next(dlg); // → Access
    await next(dlg); // → Egress
    await next(dlg); // → Barrier

    // "Barrier" matches the Field label + the every-tier note (substring).
    await expect(dlg.getByText("Barrier").first()).toBeVisible();

    // The three tiers render as their user labels — never the wire codes.
    await expect(dlg.getByText("Fence").first()).toBeVisible();
    await expect(dlg.getByText("Wall").first()).toBeVisible();
    await expect(dlg.getByText("Vault").first()).toBeVisible();

    // Every card carries the residual-risk line and the constant-note.
    await expect(dlg.getByText("Doesn't stop:").first()).toBeVisible();
    await expect(dlg.getByText(/every run still gets Wardyn's egress filtering/i)).toBeVisible();

    // Honesty: the CC1/CC2/CC3 wire codes never render as visible copy.
    await expect(dlg.getByText(/\bCC[123]\b/)).toHaveCount(0);
    // The raw substrate mechanism (gVisor) appears ONLY inside the honest
    // unavailability reason once the capability probe settles ("No Wall
    // (gVisor) runtime on this runner…") — never in the tier labels or
    // taglines. We assert on the settled DOM (probe resolved): the old
    // toHaveCount(0) raced the probe and only passed when it happened to run
    // before that legitimate unavailability reason rendered.
    await expect(dlg.getByText("Unavailable here").first()).toBeVisible(); // probe settled
    await expect(dlg.getByText(/gVisor/)).toHaveCount(1);
    await expect(dlg.getByText(/No Wall \(gVisor\) runtime on this runner/)).toBeVisible();
  });

  test("unsupported barrier tiers are disabled by runner capability", async ({ page }) => {
    const dlg = await openWizard(page);
    await fillValidBasics(dlg);
    await next(dlg); // → Access
    await next(dlg); // → Egress
    await next(dlg); // → Barrier

    // The seeded backend reports no confinement classes, so it floors to the
    // Fence: the Wall + Vault cards render a StatusChip "Unavailable here" and
    // their buttons are disabled, while the Fence card is selectable. The cards
    // are located by their unique per-tier "pick this when…" guidance copy.
    await expect(dlg.getByText("Unavailable here")).toHaveCount(2);
    await expect(dlg.getByRole("button", { name: /Trying things out/ })).toBeEnabled();
    await expect(dlg.getByRole("button", { name: /Real work on real repos/ })).toBeDisabled();
    await expect(dlg.getByRole("button", { name: /Untrusted code, or secrets nearby/ })).toBeDisabled();
  });

  test("review step shows the composed spec reflecting prior choices", async ({ page }) => {
    const dlg = await openWizard(page);

    await fillValidBasics(dlg);
    await next(dlg); // → Access
    await next(dlg); // → Egress

    // Egress: add a custom denied domain so it shows up in the composed spec.
    const denyInput = dlg.getByPlaceholder("telemetry.example.com");
    await denyInput.fill("telemetry.evil.example.com");
    await dlg.getByRole("button", { name: "Add" }).last().click();
    await expect(dlg.getByText("telemetry.evil.example.com")).toBeVisible();
    await next(dlg); // → Barrier
    await next(dlg); // → Review

    // Structured summary reflects the agent + mode + local workspace chosen.
    // exact:true — the preflight checklist rows also mention "claude-code".
    await expect(dlg.getByText("claude-code", { exact: true })).toBeVisible();
    await expect(dlg.getByText("Interactive", { exact: true })).toBeVisible();
    // The Workspace summary is the workspace itself — its name over its own
    // composition-aware sub-line (UX-4: the shared sourceSubLine, so a single
    // local dir shows its path and a multi-source workspace would read
    // "2 dirs · 1 repo") — not the synthetic `local:<basename>` wire label that
    // still travels on the run (see the runs-list assertion further down).
    await expect(dlg.getByText("payments", { exact: true })).toBeVisible();
    await expect(dlg.getByText("/home/me/projects/payments").first()).toBeVisible();

    // The verbatim inline_policy JSON is rendered and includes the denied domain,
    // the min_confinement_class wire field, and the default allowed domain.
    await expect(dlg.getByText("inline_policy (sent verbatim)")).toBeVisible();
    await expect(dlg.getByText(/telemetry\.evil\.example\.com/)).toBeVisible();
    await expect(dlg.getByText(/min_confinement_class/)).toBeVisible();
    await expect(dlg.getByText(/api\.anthropic\.com/).first()).toBeVisible();
  });

  test("Review fires preflight and renders the setup checklist (honest backend row)", async ({
    page,
  }) => {
    const dlg = await openWizard(page);
    // Ephemeral scratch run: no workspace needed — click straight to Review.
    await next(dlg); // → Access
    await next(dlg); // → Egress
    await next(dlg); // → Barrier
    const preflight = page.waitForResponse(
      (r) => r.request().method() === "POST" && /\/api\/v1\/runs\/preflight$/.test(r.url()),
    );
    await next(dlg); // → Review
    await preflight;

    // The deterministic checklist renders from the REAL preflight response.
    await expect(dlg.getByTestId("preflight-checklist")).toBeVisible();
    // The `-runner none` seeded backend can't enforce any barrier, so the Fence
    // backend row is honestly present (status "missing", never hidden) — and it
    // uses the friendly tier label, never the CC1 wire code (honesty invariant).
    await expect(dlg.getByTestId("setup-item-backend:CC1")).toBeVisible();
    await expect(dlg.getByText("Sandbox barrier: Fence")).toBeVisible();
  });

  test("save-as-profile requires a name before launching", async ({ page }) => {
    const dlg = await openWizard(page);
    await fillValidBasics(dlg);
    await next(dlg); // → Access
    await next(dlg); // → Egress
    await next(dlg); // → Barrier
    await next(dlg); // → Review

    // Turn on save-as-profile but leave the name blank → launch must surface the
    // validation error and NOT close the dialog or create a run.
    await dlg.getByText("Save as a reusable policy").click();
    await expect(dlg.getByPlaceholder("payments-interactive")).toBeVisible();
    let createFired = false;
    page.on("request", (req) => {
      if (req.method() === "POST" && /\/api\/v1\/runs$/.test(req.url())) createFired = true;
    });
    // N1: Launch is now ALSO gated behind the HIGH-risk acknowledgment (this
    // seeded backend's forced Fence/CC1 grades HIGH unconditionally) —
    // independent of, and orthogonal to, this test's own save-as-profile
    // validation below.
    await ackHighRisk(dlg);
    await dlg.getByRole("button", { name: "Launch run" }).click();
    await expect(dlg.getByText("Name the profile, or turn off save-as-profile.")).toBeVisible();
    await expect(page.getByRole("dialog")).toBeVisible();
    expect(createFired).toBe(false);
  });

  test("launching creates a run: the create call fires and the wizard closes", async ({ page }) => {
    const dlg = await openWizard(page);

    await fillValidBasics(dlg);
    await next(dlg); // → Access
    await next(dlg); // → Egress
    await next(dlg); // → Barrier
    await next(dlg); // → Review

    await expect(dlg.getByRole("button", { name: "Launch run" })).toBeVisible();
    // N1: this defaults-shaped launch still needs the acknowledgment — the
    // seeded backend's forced Fence/CC1 grades HIGH unconditionally.
    await ackHighRisk(dlg);

    const createResp = page.waitForResponse(
      (r) => r.request().method() === "POST" && /\/api\/v1\/runs$/.test(r.url()),
    );
    await dlg.getByRole("button", { name: "Launch run" }).click();
    const created = (await (await createResp).json()) as { id: string };

    // The wizard closes on success and the shell navigates straight to the
    // launched run's own detail page (every launch lands on the run it
    // launched, not just the list).
    await expect(page.getByRole("button", { name: "Launch run" })).toHaveCount(0);
    await expect(page).toHaveURL(new RegExp(`/runs/${created.id}$`));

    // THE created run (by id, not just any local:payments row — a re-run against
    // an un-reseeded backend leaves prior launches behind) exists addressably,
    // carrying the primary workspace's synthetic repo label.
    await page.goto(`/runs/${created.id}`);
    await expect(page.getByText("local:payments").first()).toBeVisible();
  });

  // N1: "Edit in wizard" used to hand a HIGH-graded proposal to a manual path
  // with no risk grade and no ack gate at all — this is the manual-wizard
  // NATIVE version of the same gap: reach a HIGH grade without ever touching
  // the AI composer. Autonomous + the untouched default never-stop lifecycle
  // is independently HIGH (risk.go:179-181, "Never-reap on a NON-interactive
  // run"); the seeded backend's forced Fence/CC1 (risk.go:108-110) also
  // contributes here, same as every other test in this file — either alone
  // would gate, so this only proves the checkbox is load-bearing, not that
  // this particular trigger is the sole cause.
  test("Review gates Launch behind an acknowledgment when the config grades HIGH (N1)", async ({
    page,
  }) => {
    const dlg = await openWizard(page);
    await fillValidBasics(dlg);

    // Autonomous needs a task (validateStep basics) — the never-stop lifecycle
    // (Confinement, step 4) is left at its default, untouched.
    await dlg.getByRole("radio", { name: "Autonomous" }).click();
    await dlg.getByPlaceholder("Describe what the agent should accomplish…").fill("Run the audit");
    await next(dlg); // → Access
    await next(dlg); // → Egress
    await next(dlg); // → Barrier

    const preflight = page.waitForResponse(
      (r) => r.request().method() === "POST" && /\/api\/v1\/runs\/preflight$/.test(r.url()),
    );
    await next(dlg); // → Review
    await preflight;

    const launchBtn = dlg.getByRole("button", { name: "Launch run" });
    await expect(dlg.getByText(/high-risk configuration/i)).toBeVisible();
    await expect(launchBtn).toBeDisabled();

    await dlg.getByRole("checkbox").click();
    await expect(launchBtn).toBeEnabled();

    const createResp = page.waitForResponse(
      (r) => r.request().method() === "POST" && /\/api\/v1\/runs$/.test(r.url()),
    );
    await launchBtn.click();
    await createResp;
    await expect(page.getByRole("button", { name: "Launch run" })).toHaveCount(0);
  });
});

// ---------------------------------------------------------------------------
// E3 default-tier prefill + E1 compare-matrix, driven through the wizard.
// The seeded backend reports no confinement classes (floors to Fence), so the E3
// cases stub /healthz to control the tier set AND seed the persisted Getting-
// started default BEFORE the app boots — the wizard's health() probe then resolves
// to a known set and resolveDefaultCc() picks the preselected card. The existing
// "floors to Fence" test above stays green: it never installs these stubs.
// ---------------------------------------------------------------------------

// Seed the persisted default barrier and stub the runner's advertised classes.
// Both must be in place before the first navigation (addInitScript) / before the
// wizard's health() fetch fires (route), so call this before openWizard().
async function withDefaultTier(page: Page, persisted: string, classes: string[]): Promise<void> {
  await page.addInitScript((cc) => {
    try {
      localStorage.setItem("wardyn-default-confinement", cc);
    } catch {
      /* private mode — ignore */
    }
  }, persisted);
  await page.route("**/healthz", (route) =>
    route.fulfill({ json: { confinement_classes: classes } }),
  );
}

// Advance an opened wizard (on Basics) to the Barrier step.
async function toBarrier(dlg: Locator): Promise<void> {
  await fillValidBasics(dlg);
  await next(dlg); // → Access
  await next(dlg); // → Egress
  await next(dlg); // → Barrier
  await expect(dlg.getByText("Barrier").first()).toBeVisible();
}

test.describe("New Run wizard — default tier prefill & compare matrix", () => {
  test("prefills the persisted default barrier when the host still runs it", async ({ page }) => {
    await withDefaultTier(page, "CC2", ["CC1", "CC2", "CC3"]);
    const dlg = await openWizard(page);
    await toBarrier(dlg);

    // Persisted Wall is available → it is the single pressed tier; Fence/Vault not.
    // Tier cards are located by their per-tier guidance copy (step-confinement's
    // TIER_GUIDANCE), the same convention the disabled-tiers test above uses.
    const wall = dlg.getByRole("button", { name: /Real work on real repos/ });
    const vault = dlg.getByRole("button", { name: /Untrusted code, or secrets nearby/ });
    await expect(wall).toHaveAttribute("aria-pressed", "true");
    await expect(dlg.getByRole("button", { name: /Trying things out/ })).toHaveAttribute(
      "aria-pressed",
      "false",
    );
    await expect(vault).toHaveAttribute("aria-pressed", "false");

    // N5/D13: the persisted pick (Wall) is honored over the strongest
    // available tier (Vault) — two true, disagreeing facts, so the screen
    // must not badge both "Recommended". The pressed Wall card owns the
    // pick; Vault, not selected, is labelled by what it actually is.
    await expect(dlg.getByText("Recommended")).toHaveCount(0);
    await expect(wall.getByText("Your saved default")).toBeVisible();
    await expect(vault.getByText("Strongest available")).toBeVisible();
  });

  test("degrades to the strongest available tier when the persisted one is gone", async ({
    page,
  }) => {
    // Persisted Vault (CC3) but the host only runs CC1/CC2 → resolveDefaultCc falls
    // to the strongest available (Wall), never leaving an unrunnable CC3 preselected.
    await withDefaultTier(page, "CC3", ["CC1", "CC2"]);
    const dlg = await openWizard(page);
    await toBarrier(dlg);

    const wall = dlg.getByRole("button", { name: /Real work on real repos/ });
    await expect(wall).toHaveAttribute("aria-pressed", "true");
    // Vault isn't runnable here → its card is disabled and unpressed.
    const vault = dlg.getByRole("button", { name: /Untrusted code, or secrets nearby/ });
    await expect(vault).toBeDisabled();
    await expect(vault).toHaveAttribute("aria-pressed", "false");

    // N5/D13: the un-runnable persisted pick can't be honored, so
    // resolveDefaultCc already fell back to the strongest available tier —
    // the two computations coincide here, so the chip stays singular.
    await expect(wall.getByText("Recommended")).toBeVisible();
    await expect(dlg.getByText("Your saved default")).toHaveCount(0);
    await expect(dlg.getByText("Strongest available")).toHaveCount(0);
  });

  test("the barrier step's 'Compare all three' opens the honest tier matrix", async ({ page }) => {
    // No stub needed — the seeded backend floors to Fence; the compare entry point
    // is present regardless of which tiers are available.
    const dlg = await openWizard(page);
    await toBarrier(dlg);

    await dlg.getByRole("button", { name: /Compare all three/ }).click();
    // The matrix opens as its own portal dialog — locate it by title so it never
    // collides with the wizard dialog behind it.
    const matrix = page.getByRole("dialog", { name: /Compare the three barriers/ });
    await expect(matrix.getByText(/Isolated from your files/).first()).toBeVisible();
    // "Needs KVM hardware" is the one approved place a substrate constraint shows.
    await expect(matrix.getByText(/Needs KVM hardware/).first()).toBeVisible();

    // Honesty (same convention as the picker test at :162-165): no wire code and no
    // raw substrate mechanism leak as visible copy inside the matrix.
    await expect(matrix.getByText(/\bCC[123]\b/)).toHaveCount(0);
    await expect(matrix.getByText(/gVisor/)).toHaveCount(0);
  });
});
