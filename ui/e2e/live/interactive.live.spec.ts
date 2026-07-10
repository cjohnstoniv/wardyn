/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// LIVE interactive-run e2e — launches an INTERACTIVE run via the manual wizard
// (interactive is the wizard's default mode; the sandbox comes up idle awaiting
// attach), asserts it reaches RUNNING with the attach affordance visible, then
// kills it (an interactive run never terminates on its own).
//
// We do NOT drive the PTY — reaching RUNNING + the attach UI is the contract.
import {
  test,
  expect,
  liveOnly,
  openManualWizard,
  makeOnboardedWorkspace,
  captureRunCreate,
  apiKillRun,
} from "./live-fixtures";

liveOnly();

test.describe.configure({ mode: "serial" });

test.describe("Interactive run (live)", () => {
  test("interactive run reaches RUNNING and shows the attach UI", async ({ page }) => {
    // Onboard BEFORE opening the dialog — see wizard.live.spec.ts for why.
    const ws = await makeOnboardedWorkspace(page, "wardyn-live-interactive");
    const dlg = await openManualWizard(page);

    // Basics: attach the onboarded workspace; Interactive is the default mode
    // (task optional).
    await dlg.getByRole("combobox", { name: /Add a workspace/ }).click();
    await dlg.page().getByRole("option", { name: new RegExp(ws.name) }).click();
    await dlg.getByRole("button", { name: "Next" }).click(); // → Access
    await expect(dlg.getByText("Anthropic auth")).toBeVisible();
    // Defaults are fine: an idle interactive sandbox needs no model credential
    // until someone attaches and drives it.
    await dlg.getByRole("button", { name: "Next" }).click(); // → Egress
    await dlg.getByRole("button", { name: "Next" }).click(); // → Barrier
    await dlg.getByRole("button", { name: "Next" }).click(); // → Review
    await expect(dlg.getByText("inline_policy (sent verbatim)")).toBeVisible();

    const runId = await captureRunCreate(page, () =>
      dlg.getByRole("button", { name: "Launch run" }).click(),
    );

    try {
      await page.goto(`/runs/${runId}`);
      await expect(page.getByText("Run ID")).toBeVisible();

      // The real sandbox boots (image may pull) and the detail page live-polls:
      // wait for the RUNNING badge, generously.
      await expect(page.getByText("Running", { exact: true })).toBeVisible({ timeout: 240_000 });

      // The attach affordance: the header chip flips to "Interactive — attachable"
      // and the Live terminal card hosts the attach terminal (not the
      // "run isn't live / autonomous" placeholder).
      await expect(page.getByText("Interactive — attachable")).toBeVisible({ timeout: 30_000 });
      await expect(page.getByText("Live terminal")).toBeVisible();
      await expect(page.getByText("This run is autonomous — the agent drives it.", { exact: false })).toHaveCount(0);
      await expect(page.getByText("The run isn't live.", { exact: false })).toHaveCount(0);
    } finally {
      // Always reap the idling sandbox — it would otherwise await attach forever.
      await apiKillRun(page, runId);
    }

    // The kill lands: the detail page's poll drops the RUNNING badge.
    await expect(page.getByText("Running", { exact: true })).toHaveCount(0, { timeout: 60_000 });
  });
});
