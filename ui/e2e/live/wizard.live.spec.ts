/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// LIVE manual-wizard e2e — the 5-step PermissionWizard (Basics → Access →
// Egress → Confinement → Review) configuring a minimal BACKGROUND run on the
// Fence (CC1, the only tier available on this box), launched against the real
// docker runner, then waited to a terminal state.
//
// Selectors mirror the hermetic ../wizard.spec.ts (same UI, real backend).
import {
  test,
  expect,
  liveOnly,
  openManualWizard,
  makeOnboardedWorkspace,
  claudeDir,
  captureRunCreate,
  waitForRunTerminal,
} from "./live-fixtures";

liveOnly();

test.describe.configure({ mode: "serial" });

test.describe("Manual wizard (live)", () => {
  test("5-step wizard → minimal Fence background run → terminal state", async ({ page }) => {
    // Onboard BEFORE opening the dialog — the wizard's WorkspacePicker fetches
    // the onboarded list once per dialog-open, so the workspace must already
    // exist server-side for it to appear in the combobox.
    const ws = await makeOnboardedWorkspace(page, "wardyn-live-wizard");
    const dlg = await openManualWizard(page);

    // Step 1 — Basics: attach the onboarded workspace, Autonomous mode, tiny task.
    await dlg.getByRole("combobox", { name: /Add a workspace/ }).click();
    await dlg.page().getByRole("option", { name: new RegExp(ws.name) }).click();
    await dlg.getByRole("radio", { name: "Autonomous" }).click();
    await dlg
      .getByPlaceholder("Describe what the agent should accomplish…")
      .fill("Create a file named hello.txt containing exactly the word hello, then stop.");
    await dlg.getByRole("button", { name: "Next" }).click();

    // Step 2 — Access: subscription auth (the proven model-access path on this
    // box; the default brokered api-key path needs a stored secret).
    await expect(dlg.getByText("Anthropic auth")).toBeVisible();
    await dlg.getByRole("radio", { name: /Subscription \(OAuth\)/ }).click();
    await dlg.getByLabel("Host ~/.claude directory").fill(claudeDir());
    await dlg.getByRole("button", { name: "Next" }).click();

    // Step 3 — Egress: keep the api.anthropic.com preset; turn OFF first-use
    // approval so an unknown domain denies deterministically instead of parking
    // the run behind a human approval no one will answer.
    await expect(dlg.getByText("Allowed domains")).toBeVisible();
    await dlg.getByRole("switch", { name: "First-use approval" }).click();
    await dlg.getByRole("button", { name: "Next" }).click();

    // Step 4 — Barrier: the Fence is the selectable default on this host.
    await expect(dlg.getByText("Barrier").first()).toBeVisible();
    await expect(dlg.getByRole("button", { name: /Trying things out/ })).toBeEnabled();
    await dlg.getByRole("button", { name: "Next" }).click();

    // Step 5 — Review → Launch run.
    await expect(dlg.getByText("inline_policy (sent verbatim)")).toBeVisible();
    const runId = await captureRunCreate(page, () =>
      dlg.getByRole("button", { name: "Launch run" }).click(),
    );
    await expect(page.getByRole("dialog")).toHaveCount(0);

    // The Runs board lists the new run; follow it to the detail hub and wait
    // for the real sandbox to finish.
    await expect(page).toHaveURL(/\/runs$/);
    await page.goto(`/runs/${runId}`);
    await expect(page.getByText("Run ID")).toBeVisible();
    await waitForRunTerminal(page, { timeout: 300_000 });
  });
});
