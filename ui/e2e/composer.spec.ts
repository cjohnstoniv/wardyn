/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// AI Run Composer ("Describe your task") e2e — the compose → review → launch flow
// reachable from the app-shell top-bar "New run" button.
//
// Source of truth read before writing selectors:
//   src/app/components/screens/new-run/{new-run-dialog,compose-form,compose-review,
//     compose-quick-review,wizard,wizard-types,step-basics,workspace-picker}.tsx
//   src/app/components/wardyn/{copy,cc-meta}.ts
//   internal/composer/{risk,compose}.go + internal/composer/backends/factory.go
//   internal/api/compose.go  (applyWorkspaces) + scripts/e2e-backend.sh
//
// The compose form's workspace field is the onboarded-workspaces multi-select
// WorkspacePicker (same control the wizard's Basics step uses) — the old required
// kind-select + free-text repo/dir inputs are gone. A raw host path / repo slug is
// no longer enterable; only pre-onboarded workspaces attach. The seeded backend
// onboards ONE workspace ("payments", a local_dir at /home/me/projects/payments).
// A workspace is now OPTIONAL when composing (empty => ephemeral scratch dir).
// The picker itself now sits behind a collapsed "context line" <details> whose
// summary states the answer (Workspace: <name> / "No workspace — …") — open it
// (openWorkspaceContext below) before interacting with the combobox or any
// per-workspace control inside it. Attachments, source URLs, the backend
// select and the clarify-mode select likewise moved behind a single "More
// options" <details> (openMoreOptions below).
//
// The seeded backend (scripts/e2e-backend.sh) wires a deterministic 'fake'
// composer with four backends:
//   fake-claude   (default) — least-privilege proposal (agent claude-code,
//                             barrier CC2 => "Wall") graded MEDIUM. applyWorkspaces
//                             overwrites the model's repo guess with the attached
//                             onboarded workspace, e.g. local:payments
//   fake-gpt                — same shape, a second provider
//   fake-risky              — proposes CC1 => "Fence" (weakest tier) graded HIGH,
//                             so the acknowledgment gate is exercised
//   fake-interview          — asks one clarifying question, then proposes
//
// REDESIGN honesty rules the review screen must hold:
//   * barriers render as the user labels Fence/Wall/Vault — the wire codes
//     CC1/CC2/CC3 NEVER leak as a bare visible label (only inside the collapsed
//     raw-JSON escape hatch, always quoted, and in native title tooltips).
//   * the run-mode pair is Interactive / Autonomous — never "Batch" — captured
//     upfront on Describe and shown on Review as a neutral fact chip, never a
//     second control (overriding it post-grade would invalidate the displayed
//     risk grade).
//   * capabilities/guarantees are split CAN ("This run can") / CAN'T ("It can't").
//   * the risk grade is deterministic ("Graded by Wardyn's rules, not the model.")
//     and ONLY a HIGH grade gates launch behind an acknowledgment.
//   * footer actions are "Approve & launch", "Edit prompt", and "Edit in wizard".
//   * a workspace pick on the first screen lands DIRECTLY on Describe — there
//     is no separate Describe/Configure chooser screen; "Configure manually"
//     is a footer button on Describe itself.
//   * every launch lands on the launched run's own detail page, not the list.
import { test, expect, gotoConsole } from "./fixtures";
import type { Page, Locator } from "@playwright/test";

// The compose + launch tests share one seeded backend; the launch test mutates
// run state (creates a run). Serial mode keeps the read-only compose/review
// assertions from racing the create, and runs the mutating test last.
test.describe.configure({ mode: "serial" });

const RISK_ATTRIBUTION = "Graded by Wardyn's rules, not the model.";

function dialog(page: Page): Locator {
  return page.getByRole("dialog");
}

// Open the shell top-bar "New run" dialog and wait for the composer-backends
// probe to resolve BEFORE interacting with the chooser. This wait is
// load-bearing, NOT belt-and-braces: chooseWorkspace (new-run-dialog.tsx)
// reads composerEnabled — derived from `backends` — AT CLICK TIME, so a click
// landing inside the probe window routes straight into the manual wizard with
// no way back to Describe, even on a control plane where the composer really
// is enabled (the workspace-step OptionCards carry `disabled={backends ===
// null}` for exactly this reason — see new-run-dialog.tsx). Clears the
// workspace-first step (Stage 3) via the "No workspace — ad-hoc run" escape,
// which now lands DIRECTLY on Describe (no intermediate chooser screen — the
// deleted "choose" screen) so every compose test below still exercises
// ComposeForm's OWN WorkspacePicker (selectWorkspace) as a separate,
// still-editable pick — mirrors wizard.spec.ts's openWizard. Returns the
// dialog already on Describe.
async function openChooser(page: Page): Promise<Locator> {
  await gotoConsole(page);
  const backends = page.waitForResponse((r) => /\/composer\/backends/.test(r.url()));
  await page.getByRole("button", { name: "New run" }).click();
  await backends;
  const dlg = dialog(page);
  await expect(dlg.getByRole("heading", { name: "New run" })).toBeVisible();
  await dlg.getByRole("button", { name: /No workspace — ad-hoc run/ }).click();
  return dlg;
}

// Open the New Run dialog on the "Describe your task" compose form — the
// ad-hoc escape now lands there directly, so this is just openChooser with an
// assertion that we're really on it.
async function openDescribe(page: Page): Promise<Locator> {
  const dlg = await openChooser(page);
  // The compose form's prompt textarea proves we're on it.
  await expect(dlg.getByLabel("Describe your task")).toBeVisible();
  return dlg;
}

// Opens ComposeForm's collapsed workspace context line (§5) — its <summary>
// reads the answer ("Workspace: <name>" vs. the honest empty-scratch line), so
// the caller says which state it expects. Every WorkspacePicker control
// (combobox, "primary" chip, "Comes with:" line, per-workspace toggles) lives
// INSIDE this <details> and is genuinely hidden — not just visually secondary
// — until it's open.
async function openWorkspaceContext(dlg: Locator, opts: { attached?: boolean } = {}): Promise<void> {
  const summary = opts.attached
    ? dlg.getByText(/^Workspace: /)
    : dlg.getByText("No workspace — an empty scratch directory inside the sandbox.");
  await summary.click();
}

// Opens the "More options" disclosure (§6) — the backend select, the
// clarify-mode select, attachments and source URLs all live inside it.
async function openMoreOptions(dlg: Locator): Promise<void> {
  await dlg.getByText("More options").click();
}

// Attach the seeded onboarded workspace ("payments", a local_dir seeded by
// scripts/e2e-backend.sh) via the WorkspacePicker combobox — the same idiom the
// wizard's Basics step uses (ui/e2e/wizard.spec.ts fillValidBasics). Options render
// in a portal OUTSIDE the dialog. Every call site reaches this from the ad-hoc
// (no workspace yet) entry, so the context line always starts on the empty
// summary — open it first.
//
// Attaching it mounts the dir READ-ONLY: the seeded "payments" workspace has no
// requirements contract at all (never scanned — see e2e-backend.sh), so it has
// no write:<path> entry, required or optional, and resolveComposeWorkspace
// (lib/api/compose.ts) resolves that to the safe read-only baseline — the SAME
// default the manual wizard's buildSpec already used (resolvedMountReadOnly,
// wizard-types.ts). There is NO read-only toggle to flip in the OTHER
// direction here: the picker's only write control is a "write to the
// directory" switch (workspace-picker.tsx) that renders solely when the
// workspace's requirements contract offers write as an OPTIONAL lane, which
// "payments" doesn't. So a read-write / HIGH-risk proposal needs a DIFFERENT
// trigger — see the fake-risky backend tests below, not this workspace.
async function selectWorkspace(page: Page, dlg: Locator): Promise<void> {
  await openWorkspaceContext(dlg);
  await dlg.getByRole("combobox", { name: /Add a workspace/ }).click();
  await page.getByRole("option", { name: /payments/ }).click();
  await expect(dlg.getByText("primary", { exact: true })).toBeVisible();
  // The picker states what the workspace carries into every run up front (the
  // "Comes with" contract line — same WorkspacePicker component the manual
  // wizard's Basics step uses, see wizard.spec.ts's fillValidBasics). The
  // seeded "payments" workspace has no requirements contract, so it reads the
  // honest empty-contract fallback.
  await expect(dlg.getByText("Comes with:")).toBeVisible();
}

// Type a prompt and Compose with NO workspace attached, landing on the "Proposed
// setup" review. A workspace is OPTIONAL now (empty => ephemeral scratch dir),
// and leaving it empty is what keeps the default proposal MEDIUM: there is no
// host mount to grade at all. Tests that need a concrete workspace on the run
// attach one themselves (composeWithWorkspace) — also MEDIUM, since the seeded
// workspace's mount resolves read-only (see selectWorkspace's doc comment); a
// HIGH-risk gate needs a different trigger (fake-risky's weak barrier tier).
async function compose(dlg: Locator, prompt: string): Promise<void> {
  await dlg.getByLabel("Describe your task").fill(prompt);
  await dlg.getByRole("button", { name: "Compose" }).click();
  await expect(dlg.getByRole("heading", { name: "Proposed setup" })).toBeVisible();
}

// As compose(), but attaching the seeded onboarded workspace — the composed run
// then carries it (repo local:payments), mounted read-only (see selectWorkspace's
// doc comment) — so this stays a MEDIUM proposal, same as compose() with no
// workspace at all, and launch needs no acknowledgment.
async function composeWithWorkspace(page: Page, dlg: Locator, prompt: string): Promise<void> {
  await dlg.getByLabel("Describe your task").fill(prompt);
  await selectWorkspace(page, dlg);
  await dlg.getByRole("button", { name: "Compose" }).click();
  await expect(dlg.getByRole("heading", { name: "Proposed setup" })).toBeVisible();
}

test.describe("AI Run Composer — Describe your task", () => {
  // Stage 3: the workspace-first pick — made BEFORE Describe/Configure is even
  // offered — must reach the compose request without the operator re-picking it
  // via ComposeForm's own WorkspacePicker (selectWorkspace, used by every other
  // test in this file after the ad-hoc escape in openChooser).
  test("picking a workspace from the entry chooser carries it straight into the compose form", async ({
    page,
  }) => {
    await gotoConsole(page);
    const backends = page.waitForResponse((r) => /\/composer\/backends/.test(r.url()));
    await page.getByRole("button", { name: "New run" }).click();
    await backends;
    const dlg = dialog(page);

    const card = dlg.getByRole("button", { name: /payments/ });
    await expect(card).toBeVisible();
    await card.click();

    // Lands DIRECTLY on Describe — no separate Describe/Configure chooser to
    // click through — already attached as the primary, no combobox interaction.
    await expect(dlg.getByLabel("Describe your task")).toBeVisible();
    await expect(dlg.getByText(/^Workspace: /)).toBeVisible();
    await openWorkspaceContext(dlg, { attached: true });
    await expect(dlg.getByText("primary", { exact: true })).toBeVisible();
    await expect(dlg.getByText("Comes with:")).toBeVisible();

    // The composed proposal is rooted in it (repo local:<base>), same as
    // composeWithWorkspace's own combobox-driven pick.
    await dlg.getByLabel("Describe your task").fill("Refactor the parser in this checkout.");
    await dlg.getByRole("button", { name: "Compose" }).click();
    await expect(dlg.getByRole("heading", { name: "Proposed setup" })).toBeVisible();
    await expect(dlg.getByText("local:payments").first()).toBeVisible();
  });

  test("the provider dropdown lists the configured backends with the default preselected", async ({
    page,
  }) => {
    const dlg = await openDescribe(page);
    await openMoreOptions(dlg);

    // The picker is a Radix Select (role="combobox") whose Field label is
    // "Which integration analyzes your task" (compose-form.tsx) — it names the
    // job, not a wire word; there has been no "Provider" label to grab.
    const provider = dlg.getByLabel("Which integration analyzes your task");
    await expect(provider).toBeVisible();
    // The default backend (fake-claude) is preselected and marked "(default)".
    await expect(provider).toContainText("fake-claude");
    await expect(provider).toContainText("(default)");

    // Opening the dropdown lists the configured backends as options.
    await provider.click();
    const listbox = page.getByRole("listbox");
    await expect(listbox.getByRole("option", { name: /fake-claude.*\(default\)/ })).toBeVisible();
    await expect(listbox.getByRole("option", { name: /fake-gpt/ })).toBeVisible();

    // Selecting a non-default backend updates the trigger.
    await listbox.getByRole("option", { name: /fake-gpt/ }).click();
    await expect(provider).toContainText("fake-gpt");
  });

  test("composing shows the Proposed Setup review with honest barrier label, CAN/CAN'T split and deterministic risk", async ({
    page,
  }) => {
    const dlg = await openDescribe(page);
    await compose(dlg, "Triage the failing CI and open a PR with a fix.");

    // The model's rationale is collapsed by default (it is the wordiest block and
    // answers "why", asked last) — expand "Why this setup" to read it. The fake
    // summary then describes a least-privilege WALL run by the user label, never
    // the CC2 wire code.
    await dlg.getByText("Why this setup").click();
    await expect(dlg.getByText(/least-privilege Wall run/i)).toBeVisible();

    // Neutral identity facts: agent + the barrier as its user label.
    await expect(dlg.getByText("Claude Code").first()).toBeVisible();
    await expect(dlg.getByText("Wall").first()).toBeVisible();
    // Honesty: no CCx wire code leaks as VISIBLE text anywhere in the review —
    // not even embedded mid-sentence. innerText excludes the collapsed
    // raw-JSON <details> (the allowed escape hatch) and title tooltips.
    expect(await dlg.innerText()).not.toMatch(/\bCC[123]\b/);
    // And the banned egress adjective never appears.
    expect(await dlg.innerText()).not.toMatch(/unrestricted/i);

    // Run mode was captured upfront on Describe and renders here as a neutral
    // fact CHIP — never "Batch", and never a second control to override it
    // (overriding it after grading would invalidate the risk grade already on
    // screen — §5).
    await expect(dlg.getByText("Autonomous", { exact: true })).toBeVisible();
    await expect(dlg.getByRole("radiogroup", { name: "Run mode" })).toHaveCount(0);

    // The CAN / CAN'T split.
    await expect(dlg.getByText("This run can")).toBeVisible();
    await expect(dlg.getByText("It can't")).toBeVisible();

    // Deterministic risk grade: MEDIUM here, attributed to Wardyn's rules.
    await expect(dlg.getByText("Risk:")).toBeVisible();
    await expect(dlg.getByText("Medium", { exact: true }).first()).toBeVisible();
    await expect(dlg.getByText(RISK_ATTRIBUTION)).toBeVisible();

    // The clamped policy keeps github.com (a default-deny baseline host).
    await expect(dlg.getByText(/github\.com/).first()).toBeVisible();
  });

  test("a medium-only proposal shows NO high-risk gate and leaves Approve & launch enabled", async ({
    page,
  }) => {
    const dlg = await openDescribe(page);
    await compose(dlg, "Add a unit test for the date parser.");

    // No high-risk section, no acknowledgment checkbox (needsAck = highItems > 0).
    await expect(page.locator('[data-testid="high-risk-section"]')).toHaveCount(0);
    await expect(dlg.getByText("High-risk configuration")).toHaveCount(0);
    await expect(dlg.getByRole("checkbox")).toHaveCount(0);

    // "Approve & launch" is ENABLED with no acknowledgment required.
    const launch = dlg.getByRole("button", { name: /Approve & launch/ });
    await expect(launch).toBeVisible();
    await expect(launch).toBeEnabled();
  });

  // Regression for the "write toggle can't make an AI-path mount read-only"
  // HIGH finding: resolveComposeWorkspace (lib/api/compose.ts) used to send
  // read_write:true unconditionally, so attaching ANY local_dir workspace —
  // including one with no write contract at all — silently mounted it
  // read-WRITE and graded HIGH. It must instead mirror the manual wizard's
  // resolvedMountReadOnly: no write:<path> entry (required or enabled
  // optional) => the safe read-only default, same as every other path.
  test("attaching an onboarded local dir with no write contract mounts read-only, not HIGH", async ({
    page,
  }) => {
    const dlg = await openDescribe(page);
    await dlg.getByLabel("Describe your task").fill("Refactor the parser in this checkout.");

    // The seeded "payments" workspace declares no requirements contract at
    // all (never scanned) — attaching it must NOT default to a writable host
    // mount just because a workspace was picked.
    await selectWorkspace(page, dlg);
    await dlg.getByRole("button", { name: "Compose" }).click();
    await expect(dlg.getByRole("heading", { name: "Proposed setup" })).toBeVisible();

    // The proposal is rooted in the local dir (repo local:<base>) but stays
    // MEDIUM — a read-only host mount grades LOW (internal/composer/risk.go),
    // so there is no HIGH item and no acknowledgment gate to clear.
    await expect(dlg.getByText("local:payments").first()).toBeVisible();
    await expect(dlg.getByText("Medium", { exact: true }).first()).toBeVisible();
    await expect(dlg.getByText("High", { exact: true })).toHaveCount(0);
    await expect(page.locator('[data-testid="high-risk-section"]')).toHaveCount(0);
    const launch = dlg.getByRole("button", { name: /Approve & launch/ });
    await expect(launch).toBeEnabled();

    // The exact-policy escape hatch is the ultimate proof: the wire mount is
    // genuinely read-only, not just quiet about it. YamlBlock renders exactly
    // one <pre> in this dialog (the collapsed "exact policy" JSON/YAML).
    await dlg.getByText("View the exact policy that will be enforced").click();
    await expect(dlg.locator("pre")).toContainText(/read_only:\s*true/);
  });

  test("a HIGH-risk proposal (weakest barrier tier) shows the acknowledgment gate", async ({
    page,
  }) => {
    // fake-risky proposes CC1 (the Fence — weakest isolation), graded HIGH, so
    // the separated high-risk section appears and launch is gated.
    const dlg = await openDescribe(page);
    await openMoreOptions(dlg);
    const provider = dlg.getByLabel("Which integration analyzes your task");
    await provider.click();
    await page.getByRole("listbox").getByRole("option", { name: /fake-risky/ }).click();
    await expect(provider).toContainText("fake-risky");

    await compose(dlg, "Run something that needs the weakest isolation tier.");

    // Overall HIGH; the barrier renders as "Fence" — never a CCx wire code in
    // any VISIBLE text (collapsed raw-JSON is excluded from innerText). Match the
    // barrier CHIP by exact text: the model-rationale prose also contains the word
    // "Fence" but sits in a collapsed <details>, so a loose substring .first()
    // would resolve to that hidden node instead of the visible chip.
    await expect(dlg.getByText("High", { exact: true }).first()).toBeVisible();
    await expect(dlg.getByText("Fence", { exact: true }).first()).toBeVisible();
    expect(await dlg.innerText()).not.toMatch(/\bCC[123]\b/);
    await expect(page.locator('[data-testid="high-risk-section"]')).toBeVisible();
    await expect(dlg.getByText(/High-risk configuration/)).toBeVisible();

    // "Approve & launch" is DISABLED until the acknowledgment is checked.
    const launch = dlg.getByRole("button", { name: /Approve & launch/ });
    await expect(launch).toBeDisabled();
    const ack = dlg.getByRole("checkbox");
    await ack.check();
    await expect(launch).toBeEnabled();
  });

  test("Cancel on the review screen closes the dialog without creating a run", async ({ page }) => {
    const dlg = await openDescribe(page);
    await compose(dlg, "Refactor the logging module.");

    let createFired = false;
    page.on("request", (req) => {
      if (req.method() === "POST" && /\/api\/v1\/runs$/.test(req.url())) createFired = true;
    });

    await dlg.getByRole("button", { name: "Cancel" }).click();
    await expect(page.getByRole("dialog")).toHaveCount(0);
    expect(createFired).toBe(false);
  });

  // §2: the retreat Review was missing — before this, Cancel destroyed the
  // session and "Edit in wizard" abandoned the conversation, so there was no
  // way to change what you asked for.
  test("Edit prompt returns to Describe with the prompt intact and re-composes cleanly", async ({
    page,
  }) => {
    const dlg = await openDescribe(page);
    const task = "Refactor the parser in this checkout.";
    await compose(dlg, task);

    await dlg.getByRole("button", { name: "Edit prompt" }).click();

    // Back on Describe, with the SAME prompt text still in the textarea.
    const prompt = dlg.getByLabel("Describe your task");
    await expect(prompt).toBeVisible();
    await expect(prompt).toHaveValue(task);

    // Nothing is broken by the retreat — composing again reaches the review
    // screen exactly as before.
    await dlg.getByRole("button", { name: "Compose" }).click();
    await expect(dlg.getByRole("heading", { name: "Proposed setup" })).toBeVisible();
  });

  test("Edit in wizard drops the proposal into the prefilled manual 5-step wizard", async ({
    page,
  }) => {
    const dlg = await openDescribe(page);
    await composeWithWorkspace(page, dlg, "Investigate the flaky integration test.");

    await dlg.getByRole("button", { name: "Edit in wizard" }).click();

    // The manual PermissionWizard takes over (its own Dialog).
    const wiz = dialog(page);
    await expect(wiz.getByRole("heading", { name: "New run" })).toBeVisible();
    await expect(wiz.getByText("Compose the agent's permission envelope.")).toBeVisible();
    for (const label of ["Basics", "Access", "Egress", "Confinement", "Review"]) {
      await expect(wiz.getByRole("button", { name: label })).toBeVisible();
    }

    // The wizard opens on Basics, PREFILLED from the proposal: the attached
    // onboarded workspace (payments), Autonomous mode (interactive=false), and the
    // task. wizardStateFromProposal re-resolves the proposal's /home/agent/work
    // mount back to the seeded workspace by source, so it lands as a real
    // attached-workspace card (name + "primary" chip + "Remove <name>" button) —
    // there is no repo text input any more.
    await expect(wiz.getByText("Workspaces", { exact: true })).toBeVisible();
    await expect(wiz.getByText("payments", { exact: true })).toBeVisible();
    await expect(wiz.getByText("primary", { exact: true })).toBeVisible();
    await expect(wiz.getByRole("button", { name: "Remove payments" })).toBeVisible();
    await expect(wiz.getByRole("radio", { name: "Autonomous" })).toBeChecked();
    await expect(wiz.getByPlaceholder("Describe what the agent should accomplish…")).toHaveValue(
      "composed by the fake backend",
    );
  });

  test("Back on Describe returns to the workspace chooser; Configure manually hands off to the wizard", async ({
    page,
  }) => {
    const dlg = await openDescribe(page);

    // Back returns to the workspace-first pick — no intermediate "choose"
    // screen any more (deleted; Configure manually now lives on Describe's
    // own footer, exercised below).
    await dlg.getByRole("button", { name: "Back" }).click();
    await expect(dlg.getByRole("button", { name: /No workspace — ad-hoc run/ })).toBeVisible();

    // Re-enter Describe and use its OWN "Configure manually" footer button.
    await dlg.getByRole("button", { name: /No workspace — ad-hoc run/ }).click();
    await dlg.getByRole("button", { name: /Configure manually/ }).click();

    // The manual wizard takes over as a CLEAN config: Basics with nothing
    // attached (no prefill). An ephemeral no-workspace run is valid, so Next
    // is enabled immediately.
    const wiz = dialog(page);
    await expect(wiz.getByText("Compose the agent's permission envelope.")).toBeVisible();
    await expect(wiz.getByText("Workspaces", { exact: true })).toBeVisible();
    await expect(wiz.getByRole("button", { name: "Review" })).toBeVisible();
    await expect(wiz.getByRole("button", { name: "Next" })).toBeEnabled();
  });

  test("interview backend asks a clarifying question, then proposes after answers", async ({
    page,
  }) => {
    const dlg = await openDescribe(page);
    await openMoreOptions(dlg);
    const provider = dlg.getByLabel("Which integration analyzes your task");
    await provider.click();
    await page.getByRole("listbox").getByRole("option", { name: /fake-interview/ }).click();
    await expect(provider).toContainText("fake-interview");

    await dlg.getByLabel("Describe your task").fill("Ship a feature and open a PR.");
    await selectWorkspace(page, dlg);
    await dlg.getByRole("button", { name: "Compose" }).click();

    // The clarify step appears (the dialog retitles to "A few questions").
    await expect(dlg.getByRole("heading", { name: "A few questions" })).toBeVisible();
    await expect(dlg.getByText("What GitHub access does this task need?")).toBeVisible();

    // Answer the single-select question and continue → the proposal review.
    await dlg.getByText("Read-only", { exact: true }).click();
    await dlg.getByRole("button", { name: "Continue" }).click();
    await expect(dlg.getByRole("heading", { name: "Proposed setup" })).toBeVisible();
  });

  test("Skip questions mode proposes one-shot even for an interview backend", async ({ page }) => {
    const dlg = await openDescribe(page);
    await openMoreOptions(dlg);
    const provider = dlg.getByLabel("Which integration analyzes your task");
    await provider.click();
    await page.getByRole("listbox").getByRole("option", { name: /fake-interview/ }).click();

    // Switch the clarify mode to "Skip questions" — the interview is bypassed.
    // The select now carries a real visible label, "Clarifying questions"
    // (compose-form.tsx §6), not the old bare aria-label "Clarify mode".
    await dlg.getByLabel("Clarifying questions").click();
    await page.getByRole("listbox").getByRole("option", { name: /Skip questions/ }).click();

    await dlg.getByLabel("Describe your task").fill("Just propose it.");
    await selectWorkspace(page, dlg);
    await dlg.getByRole("button", { name: "Compose" }).click();

    // No questions — straight to the proposal review.
    await expect(dlg.getByRole("heading", { name: "Proposed setup" })).toBeVisible();
    await expect(dlg.getByRole("heading", { name: "A few questions" })).toHaveCount(0);
  });

  // Mutating test LAST (declaration order): it creates a run.
  test("Approve & launch creates a run that then appears in the runs list", async ({ page }) => {
    const dlg = await openDescribe(page);
    // Attach the workspace so the created run carries it (asserted below). It
    // mounts read-only (see selectWorkspace's doc comment), so this stays
    // MEDIUM and launch needs no acknowledgment to clear.
    await composeWithWorkspace(page, dlg, "Bump the dependency and run the test suite.");

    const launch = dlg.getByRole("button", { name: /Approve & launch/ });
    await expect(launch).toBeEnabled();

    // The launch fires a create POST /api/v1/runs.
    const createResp = page.waitForResponse(
      (r) => r.request().method() === "POST" && /\/api\/v1\/runs$/.test(r.url()),
    );
    await launch.click();
    const created = (await (await createResp).json()) as { id: string };

    // The composer dialog closes on success and the shell navigates straight
    // to the launched run's own detail page (§7: every launch lands on the
    // run it launched, not just the list).
    await expect(page.getByRole("heading", { name: "Proposed setup" })).toHaveCount(0);
    await expect(page).toHaveURL(new RegExp(`/runs/${created.id}$`));
    // .first(): this mutating test creates a run with this task, so a re-run
    // against a non-reset backend leaves more than one (same convention as the
    // workspace assertion below).
    await expect(page.getByText("composed by the fake backend").first()).toBeVisible();
    // The created run carries the workspace we composed with (the attached
    // onboarded local dir => repo local:payments), not just the task.
    await expect(page.getByText("local:payments").first()).toBeVisible();
  });

  // Setup-readiness checklist (composer-setup-readiness plan): declared LAST —
  // it stores a real "anthropic-api-key" secret as a side effect, which would
  // change later composes' llm_access verdict if any test ran after it.
  test("the setup checklist renders llm_access as missing and flips to Configured after Add secret", async ({
    page,
  }) => {
    const dlg = await openDescribe(page);
    await compose(dlg, "Wire up a small script.");

    // The fake-claude proposal wants api.anthropic.com; the seeded operator
    // ceiling (examples/policies/demo.json) doesn't allow it and no
    // anthropic-api-key secret is seeded, so llm_access starts missing with an
    // add_secret fix (deriveSetupItems reuses the SAME reconcileLLMAccess verdict
    // the no-model-access banner shows — they can never disagree).
    const row = page.locator('[data-testid="setup-item-llm_access:claude-code"]');
    await expect(row).toBeVisible();
    await expect(row.getByText("Model access for claude-code")).toBeVisible();
    await expect(row.getByText("Needs setup")).toBeVisible();

    await row.getByRole("button", { name: "Add secret" }).click();

    // Scoped like secrets.spec.ts's addDialog: filtered on the stable write-only
    // description, since the review dialog stays open underneath this one.
    const addDlg = page.getByRole("dialog").filter({ hasText: "stored write-only" });
    await expect(addDlg).toBeVisible();
    await expect(addDlg.getByLabel("Name")).toHaveValue("anthropic-api-key");
    await addDlg.getByLabel("Value").fill("sk-ant-fake-e2e-value");
    await addDlg.getByRole("button", { name: "Save secret" }).click();
    await expect(addDlg).toHaveCount(0);

    // Decision 9 (no recheck endpoint in v1): the checklist flips the item IN
    // PLACE, client-side, from data already on hand — no re-compose, no lost
    // proposal. "Configured", never "Ready"/"Verified" (v1 is declared-present).
    await expect(row.getByText("Configured")).toBeVisible();
    await expect(dlg.getByRole("heading", { name: "Proposed setup" })).toBeVisible();
  });
});
