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
//   * the run-mode pair is Interactive / Autonomous — never "Batch".
//   * capabilities/guarantees are split CAN ("This run can") / CAN'T ("It can't").
//   * the risk grade is deterministic ("Graded by Wardyn's rules, not the model.")
//     and ONLY a HIGH grade gates launch behind an acknowledgment.
//   * footer actions are "Approve & launch" and "Edit in wizard".
import { test, expect, gotoConsole } from "./fixtures";
import type { Page, Locator } from "@playwright/test";

// The AI Run Composer is enabled (the old features.ts flag was deleted); with the
// seeded 'fake' backends this suite self-activates.
const COMPOSER_UI_ENABLED = true;

// The compose + launch tests share one seeded backend; the launch test mutates
// run state (creates a run). Serial mode keeps the read-only compose/review
// assertions from racing the create, and runs the mutating test last.
test.describe.configure({ mode: "serial" });

const RISK_ATTRIBUTION = "Graded by Wardyn's rules, not the model.";

function dialog(page: Page): Locator {
  return page.getByRole("dialog");
}

// Open the shell top-bar "New run" dialog and wait for the composer-backends
// probe to resolve BEFORE interacting with the chooser — the probe's completion
// re-sets the dialog to "choose", so clicking a chooser card mid-probe would be
// clobbered. Returns the dialog on the entry chooser.
async function openChooser(page: Page): Promise<Locator> {
  await gotoConsole(page);
  const backends = page.waitForResponse((r) => /\/composer\/backends/.test(r.url()));
  await page.getByRole("button", { name: "New run" }).click();
  await backends;
  const dlg = dialog(page);
  await expect(dlg.getByRole("heading", { name: "New run" })).toBeVisible();
  return dlg;
}

// Open the New Run dialog and switch to the "Describe your task" compose form.
async function openDescribe(page: Page): Promise<Locator> {
  const dlg = await openChooser(page);
  await dlg.getByRole("button", { name: /Describe your task/ }).click();
  // The compose form's prompt textarea proves we're on it.
  await expect(dlg.getByLabel("Describe your task")).toBeVisible();
  return dlg;
}

// Attach the seeded onboarded workspace ("payments", a local_dir seeded by
// scripts/e2e-backend.sh) via the WorkspacePicker combobox — the same idiom the
// wizard's Basics step uses (ui/e2e/wizard.spec.ts fillValidBasics). Options render
// in a portal OUTSIDE the dialog. A local dir mounts read-WRITE by default (which
// grades the mount HIGH); pass { readOnly: true } to flip the picker's Read-only
// switch so the mount grades LOW and an otherwise-MEDIUM proposal stays off the
// high-risk gate.
async function selectWorkspace(
  page: Page,
  dlg: Locator,
  opts: { readOnly?: boolean } = {},
): Promise<void> {
  await dlg.getByRole("combobox", { name: /Add a workspace/ }).click();
  await page.getByRole("option", { name: /payments/ }).click();
  await expect(dlg.getByText("primary", { exact: true })).toBeVisible();
  if (opts.readOnly) await dlg.getByRole("switch", { name: "Read-only" }).click();
}

// Type a prompt, attach the seeded onboarded workspace read-only (a read-only mount
// keeps the default proposal MEDIUM), and Compose, landing on the "Proposed setup"
// review. A workspace is OPTIONAL now (empty => ephemeral); we attach one so the
// composed run carries a concrete workspace (repo local:payments).
async function compose(page: Page, dlg: Locator, prompt: string): Promise<void> {
  await dlg.getByLabel("Describe your task").fill(prompt);
  await selectWorkspace(page, dlg, { readOnly: true });
  await dlg.getByRole("button", { name: "Compose" }).click();
  await expect(dlg.getByRole("heading", { name: "Proposed setup" })).toBeVisible();
}

test.describe("AI Run Composer — Describe your task", () => {
  test.skip(!COMPOSER_UI_ENABLED, "AI Run Composer suite disabled via the local toggle above");

  test("the provider dropdown lists the configured backends with the default preselected", async ({
    page,
  }) => {
    const dlg = await openDescribe(page);

    // The provider picker is a Radix Select (role="combobox") labelled "Provider".
    const provider = dlg.getByLabel("Provider");
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
    await compose(page, dlg, "Triage the failing CI and open a PR with a fix.");

    // The fake summary describes a least-privilege WALL run — the user label,
    // never the CC2 wire code.
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

    // The run-mode control offers Interactive / Autonomous (never "Batch").
    await expect(dlg.getByRole("radiogroup", { name: "Run mode" })).toBeVisible();
    await expect(dlg.getByRole("radio", { name: "Autonomous" })).toBeVisible();

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
    await compose(page, dlg, "Add a unit test for the date parser.");

    // No high-risk section, no acknowledgment checkbox (needsAck = highItems > 0).
    await expect(page.locator('[data-testid="high-risk-section"]')).toHaveCount(0);
    await expect(dlg.getByText("High-risk configuration")).toHaveCount(0);
    await expect(dlg.getByRole("checkbox")).toHaveCount(0);

    // "Approve & launch" is ENABLED with no acknowledgment required.
    const launch = dlg.getByRole("button", { name: /Approve & launch/ });
    await expect(launch).toBeVisible();
    await expect(launch).toBeEnabled();
  });

  test("attaching an onboarded local dir read-WRITE grades HIGH + gates launch", async ({
    page,
  }) => {
    const dlg = await openDescribe(page);
    await dlg.getByLabel("Describe your task").fill("Refactor the parser in this checkout.");

    // Attach the seeded local dir, left read-WRITE (the picker default) — the
    // operator's workspace choice adds a host mount (applyWorkspaces) that the
    // deterministic grader marks HIGH.
    await selectWorkspace(page, dlg); // read-WRITE (no Read-only toggle)
    await dlg.getByRole("button", { name: "Compose" }).click();
    await expect(dlg.getByRole("heading", { name: "Proposed setup" })).toBeVisible();

    // The proposal is rooted in the local dir (repo local:<base>), grades HIGH,
    // and launch is gated behind the acknowledgment.
    await expect(dlg.getByText("local:payments").first()).toBeVisible();
    await expect(dlg.getByText("High", { exact: true }).first()).toBeVisible();
    await expect(page.locator('[data-testid="high-risk-section"]')).toBeVisible();
    const launch = dlg.getByRole("button", { name: /Approve & launch/ });
    await expect(launch).toBeDisabled();
    await dlg.getByRole("checkbox").check(); // the review's lone ack checkbox
    await expect(launch).toBeEnabled();
  });

  test("a HIGH-risk proposal (weakest barrier tier) shows the acknowledgment gate", async ({
    page,
  }) => {
    // fake-risky proposes CC1 (the Fence — weakest isolation), graded HIGH, so
    // the separated high-risk section appears and launch is gated.
    const dlg = await openDescribe(page);
    const provider = dlg.getByLabel("Provider");
    await provider.click();
    await page.getByRole("listbox").getByRole("option", { name: /fake-risky/ }).click();
    await expect(provider).toContainText("fake-risky");

    await compose(page, dlg, "Run something that needs the weakest isolation tier.");

    // Overall HIGH; the barrier renders as "Fence" — never a CCx wire code in
    // any VISIBLE text (collapsed raw-JSON is excluded from innerText).
    await expect(dlg.getByText("High", { exact: true }).first()).toBeVisible();
    await expect(dlg.getByText("Fence").first()).toBeVisible();
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
    await compose(page, dlg, "Refactor the logging module.");

    let createFired = false;
    page.on("request", (req) => {
      if (req.method() === "POST" && /\/api\/v1\/runs$/.test(req.url())) createFired = true;
    });

    await dlg.getByRole("button", { name: "Cancel" }).click();
    await expect(page.getByRole("dialog")).toHaveCount(0);
    expect(createFired).toBe(false);
  });

  test("Edit in wizard drops the proposal into the prefilled manual 5-step wizard", async ({
    page,
  }) => {
    const dlg = await openDescribe(page);
    await compose(page, dlg, "Investigate the flaky integration test.");

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

  test("a mode switch from Describe returns to manual configuration (the wizard)", async ({
    page,
  }) => {
    const dlg = await openDescribe(page);

    // The describe form's footer "Back" returns to the entry chooser.
    await dlg.getByRole("button", { name: "Back" }).click();
    // Pick "Configure manually" from the chooser.
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
    const provider = dlg.getByLabel("Provider");
    await provider.click();
    await page.getByRole("listbox").getByRole("option", { name: /fake-interview/ }).click();
    await expect(provider).toContainText("fake-interview");

    await dlg.getByLabel("Describe your task").fill("Ship a feature and open a PR.");
    await selectWorkspace(page, dlg, { readOnly: true });
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
    const provider = dlg.getByLabel("Provider");
    await provider.click();
    await page.getByRole("listbox").getByRole("option", { name: /fake-interview/ }).click();

    // Switch the clarify mode to "Skip questions" — the interview is bypassed.
    await dlg.getByLabel("Clarify mode").click();
    await page.getByRole("listbox").getByRole("option", { name: /Skip questions/ }).click();

    await dlg.getByLabel("Describe your task").fill("Just propose it.");
    await selectWorkspace(page, dlg, { readOnly: true });
    await dlg.getByRole("button", { name: "Compose" }).click();

    // No questions — straight to the proposal review.
    await expect(dlg.getByRole("heading", { name: "Proposed setup" })).toBeVisible();
    await expect(dlg.getByRole("heading", { name: "A few questions" })).toHaveCount(0);
  });

  // Mutating test LAST (declaration order): it creates a run.
  test("Approve & launch creates a run that then appears in the runs list", async ({ page }) => {
    const dlg = await openDescribe(page);
    await compose(page, dlg, "Bump the dependency and run the test suite.");

    const launch = dlg.getByRole("button", { name: /Approve & launch/ });
    await expect(launch).toBeEnabled();

    // The launch fires a create POST /api/v1/runs.
    const createReq = page.waitForRequest(
      (req) => req.method() === "POST" && /\/api\/v1\/runs$/.test(req.url()),
    );
    await launch.click();
    await createReq;

    // The composer dialog closes on success and the shell navigates to /runs,
    // where the newly-created run (carrying the fake proposal's task) is listed.
    await expect(page.getByRole("heading", { name: "Proposed setup" })).toHaveCount(0);
    await expect(page).toHaveURL(/\/runs$/);
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
    await compose(page, dlg, "Wire up a small script.");

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
