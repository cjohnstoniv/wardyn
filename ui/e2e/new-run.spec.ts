/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New run e2e — the ONE-PAGE screen at /runs/new that replaced the 5-step
// PermissionWizard dialog (Basics → Access → Egress → Confinement → Review).
//
// What is worth proving against a live backend, rather than in jsdom: that the
// live rail actually tracks the form, and that the network dialog's selections
// reach the card and the rail. Those are the claims the redesign exists to make
// — the wizard put every consequence on a Review screen you reached last.
//
// Notes on the seeded backend (scripts/e2e-backend.sh): /healthz reports no
// confinement_classes, so the barrier floors to Fence (CC1) and Wall/Vault
// render disabled. There is no ai_provider integration at all, so an agent run
// honestly reports that no model provider is connected.
import { test, expect, gotoConsole } from "./fixtures";
import type { Page } from "@playwright/test";

async function openNewRun(page: Page) {
  await gotoConsole(page);
  await page.getByRole("button", { name: "New run" }).click();
  await expect(page).toHaveURL(/\/runs\/new$/);
  await expect(page.getByRole("heading", { name: "New run" })).toBeVisible();
}

test.describe("New run — one page", () => {
  test("the top bar's New run navigates to the page, not a dialog", async ({ page }) => {
    await openNewRun(page);
    // The wizard rendered inside a Dialog; nothing modal should be present.
    await expect(page.getByRole("dialog")).toHaveCount(0);
    await expect(page.getByRole("heading", { name: "What to run" })).toBeVisible();
  });

  // The form's fields follow the run mode. Interactive is the default, and an
  // interactive run has NO task — the server ignores one, so the screen asks
  // what to start with instead of for a prompt nothing will read.
  test("the fields follow the run mode", async ({ page }) => {
    await openNewRun(page);
    await expect(page.getByRole("radiogroup", { name: "Start with" })).toBeVisible();
    await expect(page.getByLabel("Task")).toHaveCount(0);

    await page.getByRole("radio", { name: /^Autonomous/ }).click();
    await expect(page.getByLabel("Task")).toBeVisible();
    await expect(page.getByRole("radiogroup", { name: "Start with" })).toHaveCount(0);
  });

  // The choice that proves a run needn't involve AI. It re-labels the field and
  // swaps the help text, because a shell command is run verbatim — and it is
  // unattended by definition, so the run mode disappears with the agent picker.
  test("Shell command drops the agent picker and relabels the field", async ({ page }) => {
    await openNewRun(page);
    await page.getByRole("radio", { name: "Shell command" }).click();
    await expect(page.getByLabel("Command")).toBeVisible();
    await expect(page.getByText(/Run verbatim in the sandbox/)).toBeVisible();
    await expect(page.getByRole("combobox", { name: "Agent" })).toHaveCount(0);
    await expect(page.getByRole("radiogroup", { name: "Run mode" })).toHaveCount(0);
  });

  // The rail is the whole point: it answers "what can this run do" while you
  // build it, not on a screen you reach at the end.
  test("the rail tracks the network choice as it changes", async ({ page }) => {
    await openNewRun(page);
    const rail = page.getByText("What this run can do").locator("..");

    await page.getByRole("radio", { name: /^None/ }).click();
    await expect(rail.getByText("0 hosts allowed")).toBeVisible();

    await page.getByRole("radio", { name: /Common package registries/ }).click();
    await expect(rail.getByText(/1[0-9] hosts allowed/)).toBeVisible();
    await expect(rail.getByText("github.com")).toBeVisible();

    await page.getByRole("radio", { name: /^Everything/ }).click();
    await expect(rail.getByText(/Open egress\. Nothing is blocked\./)).toBeVisible();
  });

  // Recording is allow-everything by definition, and on the weakest barrier
  // that is worth saying out loud rather than leaving the operator to infer.
  test("Record states its own blast radius, and warns on Fence", async ({ page }) => {
    await openNewRun(page);
    await page.getByRole("radio", { name: /^Record/ }).click();
    await expect(page.getByText(/That is what recording means/)).toBeVisible();
    await expect(page.getByText(/every host this run reaches is logged and becomes the policy/i)).toBeVisible();
    // The seeded backend floors to Fence, so the CC1 caveat must be showing.
    await expect(page.getByText(/an unrestricted run can move your data out/i)).toBeVisible();
  });

  test("Edit hosts… opens the dialog and its choice reaches the card and rail", async ({ page }) => {
    await openNewRun(page);
    await page.getByRole("radio", { name: /Common package registries/ }).click();
    await page.getByRole("button", { name: "Edit hosts…" }).click();

    const dlg = page.getByRole("dialog");
    await expect(dlg.getByText("Network for this run")).toBeVisible();

    // The run's most consequential choice, as three cards instead of a Select.
    await dlg.getByRole("radio", { name: /Deny silently/ }).click();
    await dlg.getByRole("button", { name: /crates\.io/ }).click(); // drop one host
    await dlg.getByRole("button", { name: "Save hosts" }).click();

    await expect(page.getByRole("dialog")).toHaveCount(0);
    // The card stops asserting a preset the run no longer uses.
    await expect(page.getByText(/Edited — this run uses your host list/)).toBeVisible();
    await expect(
      page.getByRole("radiogroup", { name: "Unlisted hosts" }).getByRole("radio", { name: "Deny silently" }),
    ).toBeChecked();
  });

  test("Cancel in the dialog changes nothing", async ({ page }) => {
    await openNewRun(page);
    await page.getByRole("radio", { name: /Common package registries/ }).click();
    await page.getByRole("button", { name: "Edit hosts…" }).click();
    const dlg = page.getByRole("dialog");
    await dlg.getByRole("radio", { name: /Deny silently/ }).click();
    await dlg.getByRole("button", { name: "Cancel" }).click();

    await expect(page.getByRole("dialog")).toHaveCount(0);
    await expect(page.getByText(/Edited — this run uses your host list/)).toHaveCount(0);
  });

  // The "no model provider is connected" rail warning is NOT asserted here on
  // purpose. Whether one exists is an environment fact: this backend runs
  // wardynd on the host, and setupProviders() detects the host's own logged-in
  // Claude CLI — so the warning is correctly absent on a developer machine and
  // present on a bare CI box. Asserting either way would make this suite pass
  // or fail on who ran it. It is pinned in new-run-screen.test.tsx instead,
  // where the SetupStatus is controlled.

  // Every run is named: the title is the grouping key on the Runs board, so
  // Launch stays disabled — and says why — until there is one.
  test("Launch waits for a title, and says what it is waiting for", async ({ page }) => {
    await openNewRun(page);
    const launch = page.getByRole("button", { name: "Launch run" });
    await expect(launch).toBeDisabled();
    await expect(page.getByText("Give this run a title.")).toBeVisible();

    await page.getByLabel("Title").fill("e2e smoke");
    await expect(launch).toBeEnabled();
  });

  test("launching creates a run and lands on its detail page", async ({ page }) => {
    await openNewRun(page);
    await page.getByLabel("Title").fill("e2e smoke");
    await page.getByRole("button", { name: "Launch run" }).click();
    await expect(page).toHaveURL(/\/runs\/[0-9a-f-]{8,}/, { timeout: 15_000 });
  });
});
