/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// LIVE composer e2e — the headline: "Describe your task" against the REAL
// host-mode stack (scripts/run-host.sh: real Opus composer + docker runner).
// Composes a tiny one-file static-site task into an ephemeral workspace with
// the per-run "Use my Claude subscription" opt-in ON, approves the proposal,
// and waits for the REAL sandboxed run to reach COMPLETED.
//
// Selectors mirror the hermetic ../composer.spec.ts (same UI, real backend).
import {
  test,
  expect,
  liveOnly,
  openNewRunChooser,
  captureRunCreate,
  waitForRunTerminal,
} from "./live-fixtures";
import { COMPOSER_UI_ENABLED } from "../../src/app/lib/features";

liveOnly();

// One real stack; a real run mutates it. Keep this file single-threaded.
test.describe.configure({ mode: "serial" });

// Small on purpose: one file, no build tooling, no network — minimal model time.
const PROMPT =
  "Create a single index.html file containing an <h1> that says 'Hello from Wardyn' " +
  "and one short paragraph. One file only, no build tools, no network access needed.";

test.describe("AI Run Composer (live)", () => {
  test.skip(!COMPOSER_UI_ENABLED, "AI Run Composer UI is flag-off (features.ts) — suite self-activates when the flag flips");

  test("describe → subscription on → compose → approve & launch → COMPLETED", async ({ page }) => {
    const dlg = await openNewRunChooser(page);
    await dlg.getByRole("button", { name: /Describe your task/ }).click();
    await expect(dlg.getByLabel("Describe your task")).toBeVisible();

    await dlg.getByLabel("Describe your task").fill(PROMPT);

    // Background so the run terminates on its own (interactive would idle).
    await dlg.getByRole("radio", { name: /^Background/ }).click();

    // Per-run opt-in: subscription model access is the proven path on this box.
    const sub = dlg.getByRole("switch", { name: "Use my Claude subscription" });
    if (!(await sub.isChecked())) await sub.click();
    await expect(sub).toBeChecked();

    // Ephemeral scratch workspace — nothing on the host to prepare or clean up.
    await dlg.getByLabel("Workspace").click();
    await page.getByRole("listbox").getByRole("option", { name: /Ephemeral/ }).click();

    // Skip clarifying questions: one-shot compose.
    await dlg.getByLabel("Clarify mode").click();
    await page.getByRole("listbox").getByRole("option", { name: /Skip questions/ }).click();

    // Compose — a REAL model call analyzes the task (generous wait).
    await dlg.getByRole("button", { name: "Compose" }).click();
    await expect(dlg.getByRole("heading", { name: "Proposed setup" })).toBeVisible({
      timeout: 180_000,
    });

    // The review's warnings/notes must surface the deterministic model-access
    // ground-truth line (reconcileLLMAccess): "model access provisioned…" or an
    // honest "no model access…" — either way the line is present for a Claude run.
    await expect(dlg.getByText(/model access/i).first()).toBeVisible();

    // The run mode we chose upfront carried into the proposal.
    await expect(dlg.getByRole("radio", { name: "Autonomous" })).toBeChecked();

    // A HIGH-graded proposal gates launch behind the lone ack checkbox — check
    // it if present (subscription cred mounts can grade HIGH).
    const ack = dlg.getByRole("checkbox");
    if ((await ack.count()) > 0) await ack.first().check();

    const launch = dlg.getByRole("button", { name: /Approve & launch/ });
    await expect(launch).toBeEnabled();

    const runId = await captureRunCreate(page, () => launch.click());
    await expect(page.getByRole("dialog")).toHaveCount(0);

    // Follow the run on its detail page until the REAL sandbox finishes.
    await page.goto(`/runs/${runId}`);
    await expect(page.getByText("Run ID")).toBeVisible();
    await waitForRunTerminal(page, { timeout: 300_000 });

    // Terminal success state on the detail hub, kill disabled.
    await expect(page.getByText("Completed", { exact: true }).first()).toBeVisible();
    await expect(page.getByRole("button", { name: "Kill", exact: true })).toBeDisabled();
  });
});
