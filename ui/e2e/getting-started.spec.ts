/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, gotoConsole, navTo } from "./fixtures";

// Getting Started funnel — hermetic walk against the seeded backend (real
// wardynd + Postgres + `none` runner, admin-token auth). The unit suites cover
// the branchy per-step logic; this spec proves the real wiring: sidebar entry →
// onboarding tour → SetupScreen, the full 13-step Next walk in STEP_ORDER
// (essentials [environment, corp_network, integrations] → demos [4 keyless
// sub-steps + the fifth, harness-aware one] → your work [sources, images,
// workspaces] → finish [review, launch]), honest not-ready gating on a
// runner-less host, and the Finish-later dismissal.
//
// 13 -> 9 collapse: the old provider/host_proxy/scm_provider/artifact_repo/
// credentials steps are gone — that configuration now lives on /integrations,
// and Getting Started embeds a thin, linked view of it as ONE Integrations
// step instead.
//
// 9 -> 10: `corp_network` came back as its own step, right before
// Integrations (host proxy + egress redirection moved OFF Integrations onto
// it) — see steps.ts's PHASES and corp-network.spec.ts for that step's own
// dedicated coverage; this walk only needs its heading in the right slot.
//
// 10 -> 12: `workspaces` (the aggregate) split into its three tiers —
// `sources` (Directories & repos), `images` (Base images), and `workspaces`
// itself — walked in that dependency order.
//
// 12 -> 13: `agent-in-the-box` joined as the LAST Demos step — the catalog's
// fifth (harness-aware) demo, previously /demos-only, now always on the rail.
// This hermetic backend has no AI provider connected, so it renders its
// LOCKED body here (an invitation, no Start button) — the live body has its
// own coverage in harness-demo-step.test.tsx (a real provider needs seeding
// this suite's shared backend can't do safely; see that file's own header).
//
// Note the seeded backend's shape is load-bearing here: driver "none" means no
// barrier is ready and no model is connected, so the launch gate MUST be
// closed — if this host ever gains a runner, the gating assertions below are
// the ones to revisit.
//
// Corporate network's Next is now a hard gate too (steps.ts's
// corpNetworkGate) — this walk can no longer blindly Next through it.
// The seeded `-runner none` backend answers a REAL Test-proxy click with
// "no_runner", the ladder's one honest bypass, exactly as an operator on a
// runner-less host would see — so passCorpNetworkGate() below clicks the real
// button rather than stubbing the endpoint. Without that click the gate stays
// shut (proxyProbe starts undefined, not no_runner), so every walk below
// would otherwise stall clicking a disabled Next on step 2.

// Fresh tour every test: the wardyn-onboarding-seen flag is what swaps the
// tour for the SetupScreen, and specs must not depend on each other's flags.
async function openSetupFunnel(page: import("@playwright/test").Page) {
  await gotoConsole(page);
  await navTo(page, "Getting started");
  // The welcome hero (onboarding tour) renders first on a fresh session.
  await expect(page.getByRole("button", { name: /get started|finish setup/i })).toBeVisible();
  await page.getByRole("button", { name: /get started|finish setup/i }).click();
  // The SetupScreen funnel replaces the hero.
  await expect(page.getByRole("heading", { name: "Getting started" })).toBeVisible();
  await expect(page.getByText(/step 1 of 13/i)).toBeVisible();
}

// Clears Corporate network's connectivity gate for real (no route stub): this
// backend genuinely runs `-runner none`, so clicking Test connectivity gets back
// {state:"no_runner"} from the actual server (internal/api/site_config_probe.go
// short-circuits on s.cfg.Runner == nil before it ever tries to launch a
// probe) — corpNetworkGate's one honest bypass, which clears the whole ladder
// at once (no Egress-tab visit needed). Must be called while the
// Corporate network step is on screen, on its default Host proxy tab.
async function passCorpNetworkGate(page: import("@playwright/test").Page) {
  const main = page.getByRole("main");
  await main.getByRole("button", { name: /^test connectivity$/i }).click();
  await expect(main.getByText(/can't test here/i)).toBeVisible();
  // The forward walk passes THROUGH Egress redirection (navigation, not a
  // gate) — step through it here so the walks' next "Next:" click advances
  // to Integrations, same as before.
  await page.getByRole("button", { name: /^next: egress redirection$/i }).click();
  await expect(main.getByText(/nothing redirected on this host/i)).toBeVisible();
}

test.describe("Getting Started funnel", () => {
  test("walks all thirteen steps via Next in STEP_ORDER", async ({ page }) => {
    await openSetupFunnel(page);
    const main = page.getByRole("main");
    const nextBtn = page.getByRole("button", { name: /^Next:/i });

    // STEP_ORDER (steps.ts PHASES): essentials [environment, corp_network,
    // integrations] → demos(4 + the fifth, harness-aware one) → your work
    // [sources, images, workspaces] → finish [review, launch]. Corporate
    // network (host proxy + egress redirection) sits between the barrier and
    // Integrations; Integrations itself keeps only the model/harness +
    // SCM-host picker.
    // 1 environment
    await expect(main.getByRole("heading", { name: /pick your barrier/i })).toBeVisible();
    // 2 corporate network
    await nextBtn.click();
    await expect(main.getByRole("heading", { name: /corporate network/i })).toBeVisible();
    await passCorpNetworkGate(page);
    // 3 integrations
    await nextBtn.click();
    await expect(
      main.getByRole("heading", { name: /connect what's outside wardyn/i }),
    ).toBeVisible();
    // 4-7 demos: the four keyless demo sub-steps (heading = the demo title)
    await nextBtn.click();
    await expect(main.getByRole("heading", { name: /the sealed box/i })).toBeVisible();
    await nextBtn.click();
    await expect(main.getByRole("heading", { name: /fail, then approve/i })).toBeVisible();
    await nextBtn.click();
    await expect(main.getByRole("heading", { name: /held at the door/i })).toBeVisible();
    await nextBtn.click();
    await expect(main.getByRole("heading", { name: /lines that can't be crossed/i })).toBeVisible();
    // 8 the fifth (harness-aware) demo — LOCKED here: this hermetic backend has
    // no AI provider connected, so it's the invitation panel, not the live
    // catalog body — no "Start demo" button (see harness-demo-step.test.tsx
    // for the live-state coverage this shared backend can't safely seed).
    await nextBtn.click();
    await expect(main.getByRole("heading", { name: /the agent in the box/i })).toBeVisible();
    await expect(main.getByRole("button", { name: /^go to integrations$/i })).toBeVisible();
    await expect(main.getByRole("button", { name: /^start demo$/i })).toHaveCount(0);
    // 9-11 your work: dirs/repos, base images, then the workspace tier
    await nextBtn.click();
    await expect(main.getByRole("heading", { name: /^directories & repos$/i })).toBeVisible();
    await nextBtn.click();
    await expect(main.getByRole("heading", { name: /^base images$/i })).toBeVisible();
    await nextBtn.click();
    await expect(main.getByRole("heading", { name: /onboard a workspace/i })).toBeVisible();
    // 12-13 finish: review → launch
    await nextBtn.click();
    await expect(main.getByRole("heading", { name: /review readiness/i })).toBeVisible();
    await nextBtn.click();
    await expect(main.getByRole("heading", { name: /launch your first run/i })).toBeVisible();
    await expect(page.getByText(/step 13 of 13/i)).toBeVisible();
  });

  test("launch stays gated on a runner-less host (no fake green)", async ({ page }) => {
    await openSetupFunnel(page);
    const main = page.getByRole("main");

    // The environment step tells the truth about the seeded `none` runner.
    await expect(main.getByText(/no sandbox runner/i)).toBeVisible();

    // Footer on the last step: the launch button is disabled with the
    // essentials helper visible.
    const nextBtn = page.getByRole("button", { name: /^Next:/i });
    // 12 Next clicks total (12 -> 13: agent-in-the-box joined the walk): 1 to
    // reach corp_network, then 11 more once its gate is cleared — see
    // passCorpNetworkGate above.
    await nextBtn.click();
    await passCorpNetworkGate(page);
    for (let i = 0; i < 11; i++) await nextBtn.click();
    await expect(main.getByRole("heading", { name: /launch your first run/i })).toBeVisible();
    for (const btn of await page.getByRole("button", { name: /launch your first run/i }).all()) {
      await expect(btn).toBeDisabled();
    }
    await expect(main.getByText(/a sandbox barrier is required first/i)).toBeVisible();
  });

  test("'Finish setup' at the flow's end completes to Runs; no early exit", async ({ page }) => {
    await openSetupFunnel(page);
    // No early "Finish later" escape any more — the mandatory gate keeps the
    // operator in setup until the end (this seeded backend is admin-token, not
    // local, so nav itself isn't hidden — but the escape verb is still gone).
    await expect(page.getByRole("button", { name: /finish later/i })).toHaveCount(0);
    // Walk to the final (Launch) step and complete via "Finish setup". 12 Next
    // clicks total (see the gated-launch test above for the count) — see
    // passCorpNetworkGate above.
    const nextBtn = page.getByRole("button", { name: /^Next:/i });
    await nextBtn.click();
    await passCorpNetworkGate(page);
    for (let i = 0; i < 11; i++) await nextBtn.click();
    await page.getByRole("button", { name: /^finish setup$/i }).click();
    // Completion lands on Runs...
    await expect(page.getByRole("heading", { name: "Runs", level: 1 })).toBeVisible();
    // ...and Getting started remains reachable from the sidebar (straight to the
    // funnel — the tour is one-shot).
    await navTo(page, "Getting started");
    await expect(page.getByRole("heading", { name: "Getting started" })).toBeVisible();
    await expect(page.getByText(/step 1 of 13/i)).toBeVisible();
  });
});
