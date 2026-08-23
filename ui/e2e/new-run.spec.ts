/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New run e2e — the ONE-PAGE screen at /runs/new that replaced the 5-step
// PermissionWizard dialog (Basics → Access → Egress → Confinement → Review).
//
// What is worth proving against a live backend, rather than in jsdom: that the
// form's fields follow the run mode, and that the shared Policy panel — the
// same component /policies authors through — really governs what this run
// ships. The Confinement + Network cards it replaced put the envelope behind a
// preset stack and a dialog; the spec JSON is the envelope now.
//
// Notes on the seeded backend (scripts/e2e-backend.sh): wardynd runs with
// -runner none, so /healthz advertises NO confinement_classes — unknown, not
// confirmed-absent, so all three barrier tiers stay selectable and the runner
// capability gate (runs_create.go) is skipped entirely. There is no ai_provider
// integration at all, so an agent run honestly reports that no model provider
// is connected.
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

  // The Policy panel replaced the Confinement + Network cards: the run's policy
  // IS the spec JSON, authored through the same component /policies uses. Its
  // live derivations are what the rail's Network section used to be — the
  // consequences of the envelope, while you build it rather than after.
  test("the panel's derivations track the spec as it changes", async ({ page }) => {
    await openNewRun(page);
    const spec = page.getByLabel("Spec (JSON)");

    // Opens on the Minimal template: one host, a review rule, a CC2 floor.
    await expect(spec).toHaveValue(/"api\.anthropic\.com"/);
    await expect(page.getByText("Valid JSON")).toBeVisible();
    await expect(page.getByText("1 domain allowed")).toBeVisible();

    await page.getByRole("button", { name: "Package registries" }).click();
    await expect(spec).toHaveValue(/"pypi\.org"/);
    await expect(page.getByText(/1[0-9] domains allowed/)).toBeVisible();

    // A broken document says so instead of deriving from nothing, and Launch
    // stops rather than posting a body nobody can read. The title is filled
    // first so the disable is the SPEC's doing, not the title rule's.
    await page.getByLabel("Title").fill("e2e smoke");
    await expect(page.getByRole("button", { name: "Launch run" })).toBeEnabled();
    await spec.fill("{ not json");
    await expect(page.getByText(/Invalid JSON/)).toBeVisible();
    await expect(page.getByRole("button", { name: "Launch run" })).toBeDisabled();
    await expect(page.getByText("The policy spec isn't valid JSON.")).toBeVisible();
  });

  // The Record radio only ever set allow_all_egress — a promise this screen
  // could not keep, since real Record Mode is workspace-level. It is a template
  // now, named for what it actually does.
  test("the allow-all template says block-list only, never 'unrestricted'", async ({ page }) => {
    await openNewRun(page);
    await page.getByRole("button", { name: "Allow-all — observe first" }).click();

    await expect(page.getByLabel("Spec (JSON)")).toHaveValue(/"allow_all_egress": true/);
    await expect(page.getByText("Allow-all egress (block-list only)")).toBeVisible();
  });

  // The Edit-hosts dialog's job — pick hosts, set the unlisted rule, block hosts
  // outright — is the JSON itself now, with the Fields rail documenting each key
  // and writing a starting value for it.
  test("the Fields rail inserts a key into the spec, and the derivations follow", async ({ page }) => {
    await openNewRun(page);
    const spec = page.getByLabel("Spec (JSON)");

    await page.getByRole("button", { name: "Insert denied_domains" }).click();
    await expect(spec).toHaveValue(/"denied_domains"/);
    // A deny beats an allow in both egress modes, so it is counted separately.
    await expect(page.getByText("1 domain allowed, 1 denied")).toBeVisible();

    // The unlisted-host rule is a documented key, not a buried dropdown.
    await expect(page.getByTitle("docs/POLICIES.md#first_use_approval-modes")).toBeVisible();
  });

  // Two lanes, one panel: reuse a stored policy by reference, or author one for
  // this run. Switching lanes swaps the editor for the picker, and nothing on
  // the page is merged into a stored spec.
  test("the mode row swaps the editor for the saved-policy picker", async ({ page }) => {
    await openNewRun(page);
    await expect(page.getByLabel("Spec (JSON)")).toBeVisible();

    await page.getByRole("button", { name: /Reuse a saved policy/ }).click();
    await expect(page.getByLabel("Spec (JSON)")).toHaveCount(0);
    await expect(page.getByRole("combobox", { name: "Saved policy" })).toBeVisible();
    // Nothing is picked yet, so Launch says what it is waiting for.
    await expect(page.getByText("Pick a saved policy, or write a custom one.")).toBeVisible();

    await page.getByRole("button", { name: /Custom policy/ }).click();
    await expect(page.getByLabel("Spec (JSON)")).toBeVisible();
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
