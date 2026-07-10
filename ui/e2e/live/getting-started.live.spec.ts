/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// LIVE Getting-started e2e — asserts the setup funnel's capability detection
// reflects the REAL box, cross-checked against GET /api/v1/setup/status rather
// than hardcoding this machine: Fence (CC1) must be genuinely available on a
// docker-runner host stack; missing tiers must render their honest state
// ("Needs setup" for a fixable Wall, "Unavailable here" + the Kata reason for
// Vault on a managed-VM Docker like Docker Desktop/WSL2).
//
// Selectors from setup-screen.tsx / llm-access.tsx / status-chip.tsx.
import { test, expect, liveOnly, gotoConsole, AUTH_HEADERS } from "./live-fixtures";

liveOnly();

type SetupStatus = {
  runner: { confinement_classes: string[]; confinement_substrates?: Record<string, string> };
  platform: { os: string; wsl: boolean; kvm?: boolean };
};

test.describe("Getting started (live)", () => {
  test("capability detection reflects the real host", async ({ page }) => {
    await gotoConsole(page);

    // Ground truth from the daemon itself.
    const resp = await page.request.get("/api/v1/setup/status", { headers: AUTH_HEADERS });
    expect(resp.ok()).toBeTruthy();
    const status = (await resp.json()) as SetupStatus;
    const classes = status.runner.confinement_classes ?? [];
    // A live host-mode stack runs the docker runner — the Fence must be real.
    expect(classes).toContain("CC1");

    await page.getByRole("link", { name: /^Getting started/ }).click();
    await expect(
      page.getByRole("heading", { name: "Getting started" }),
    ).toBeVisible();

    // The funnel opens on the Environment step: the barrier picker.
    const main = page.getByRole("main");
    await expect(main.getByText("Pick your barrier")).toBeVisible();
    for (const tier of ["Fence", "Wall", "Vault"]) {
      await expect(main.getByText(tier, { exact: true }).first()).toBeVisible();
    }

    // Fence available ⇒ at least one Ready chip in the barrier list. (The
    // initial recheck may briefly show "Checking…", so wait it out.)
    await expect(main.getByText("Ready", { exact: true }).first()).toBeVisible({
      timeout: 30_000,
    });

    // Wall (CC2) missing ⇒ an honest, fixable "Needs setup" — never a fake Ready.
    if (!classes.includes("CC2")) {
      await expect(main.getByText("Needs setup", { exact: true }).first()).toBeVisible();
    }

    // Vault (CC3): the /dev/kvm HARDWARE fact decides — a KVM-less host reads an
    // honest "Incompatible here" (with the concrete why); a KVM-capable host
    // that merely lacks the runtime reads a fixable "Needs setup" instead.
    if (!classes.includes("CC3")) {
      if (status.platform.kvm === false) {
        await expect(main.getByText("Incompatible here", { exact: true })).toBeVisible();
        await expect(main.getByText(/doesn't expose \/dev\/kvm/).first()).toBeVisible();
      } else {
        await expect(main.getByText("Needs setup", { exact: true }).first()).toBeVisible();
      }
    }
  });

  // E2/E3 additions — both Fence-only-safe: E2 asserts against whatever
  // /setup/status actually reports (never a hardcoded tier), E3 proves persistence
  // in this browser. (E1's compare-matrix dialog was removed from Getting Started —
  // TierMatrixDialog still lives in the New Run wizard, covered by wizard.spec.ts.)
  test("substrate provenance and a saved default barrier (live)", async ({ page }) => {
    await gotoConsole(page);

    const resp = await page.request.get("/api/v1/setup/status", { headers: AUTH_HEADERS });
    expect(resp.ok()).toBeTruthy();
    const status = (await resp.json()) as SetupStatus;
    const classes = status.runner.confinement_classes ?? [];
    expect(classes).toContain("CC1"); // Fence is genuinely real on a docker-runner host

    await page.getByRole("link", { name: /^Getting started/ }).click();
    await expect(page.getByRole("heading", { name: "Getting started" })).toBeVisible();
    const main = page.getByRole("main");
    await expect(main.getByText("Pick your barrier")).toBeVisible();
    // Wait out the initial recheck so the barrier list has settled (Ready chip).
    await expect(main.getByText("Ready", { exact: true }).first()).toBeVisible({ timeout: 30_000 });

    // E2 — provenance: if the daemon reports a substrate for the always-ready Fence,
    // the card names the concrete runtime it runs as. Asserted against whatever the
    // status carries, never a hardcoded value.
    if (status.runner.confinement_substrates?.CC1) {
      await expect(main.getByText(/Running here as/).first()).toBeVisible();
    }

    // E3 — persistence: selecting the ready Fence radio saves it as the default
    // barrier for new runs, proven by reading it back out of this browser's
    // localStorage. Fence is always ready, so this is host-independent. The
    // protection-matrix picker is a radiogroup now (role=radio, aria-checked) —
    // not a button with aria-pressed.
    const fence = main.getByRole("radio", { name: /Fence/ });
    await fence.click();
    await expect(fence).toHaveAttribute("aria-checked", "true");
    const saved = await page.evaluate(() => localStorage.getItem("wardyn-default-confinement"));
    expect(saved).toBe("CC1");
  });

  // P4 — the renamed Model/Harness Provider step (detection-driven family
  // grouping) and the three enterprise steps (Host Proxy, Artifact Redirect, SCM
  // Provider — the redesigned STEP_ORDER walks Artifact Redirect before SCM
  // Provider) render in order. Walked live so the new funnel structure is proven
  // end to end against the real /setup/status + /site-config, not just in jsdom.
  test("the funnel exposes the provider families and the enterprise steps (live)", async ({
    page,
  }) => {
    await gotoConsole(page);
    await page.getByRole("link", { name: /^Getting started/ }).click();
    await expect(page.getByRole("heading", { name: "Getting started" })).toBeVisible();
    const main = page.getByRole("main");
    await expect(main.getByText("Pick your barrier")).toBeVisible();

    // The provider step was renamed "Provider" -> "Model/Harness Provider" (the
    // rail label is visible from the first step). Scoped to the rail's own nav
    // landmark ("Setup steps") — the compact lg-only icon rail carries the same
    // label text in a sr-only span, which would otherwise double-match a
    // main-scoped getByText.
    const rail = main.getByRole("navigation", { name: "Setup steps" });
    await expect(rail.getByText("Model/Harness Provider")).toBeVisible();

    const nextBtn = page.getByRole("button", { name: /^Next:/i });

    // Environment -> Model/Harness Provider: two detection-driven family groups.
    await nextBtn.click();
    await expect(
      main.getByRole("heading", { name: /connect a model or agent harness/i }),
    ).toBeVisible();
    await expect(main.getByText("Claude / Anthropic")).toBeVisible();
    await expect(main.getByText("OpenAI / Codex")).toBeVisible();

    // -> Host Proxy -> Artifact Redirect -> SCM Provider: the redesigned
    // STEP_ORDER walks the corporate phase (Host Proxy, Artifact Redirect)
    // before the work phase's SCM Provider step — each new step's heading in
    // that order.
    await nextBtn.click();
    await expect(main.getByRole("heading", { name: /corporate host proxy/i })).toBeVisible();
    await nextBtn.click();
    await expect(
      main.getByRole("heading", { name: /artifact registry redirection/i }),
    ).toBeVisible();
    await nextBtn.click();
    await expect(main.getByRole("heading", { name: /source control provider/i })).toBeVisible();
  });
});
