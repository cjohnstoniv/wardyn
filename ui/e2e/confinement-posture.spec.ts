/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, gotoConsole } from "./fixtures";
import type { Page } from "@playwright/test";

// #162 — the console never said when network confinement was unenforced.
//
// The premise correction from the mock-approval comment: /healthz omits
// `network_policy` on BOTH Docker (not applicable) and a k8s daemon that
// could not confirm the verdict (must warn) — one empty string, two opposite
// meanings — so the shell resolves posture from `runner` + `network_policy`
// together. This walks all five rows of that table against one intercepted
// /healthz, real everything else (the seeded backend's own runs board).
async function mockHealthz(page: Page, body: { runner: string; network_policy?: string }): Promise<void> {
  await page.route("**/healthz", (route) =>
    route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ status: "ok", trust_domain: "e2e.test", identity_provider: "embedded", ...body }),
    }),
  );
}

// The board's own ConfinementChip (run-card.tsx) — any run works, the case
// under test never depends on which one. `internal class` is a substring only
// this chip's title ever carries (primitives.tsx#ConfinementChip).
function firstConfinementChip(page: Page) {
  return page.locator('[title*="internal class"]').first();
}

const NO_BANNER = /network confinement|network-confined/i;

test.describe("confinement posture (#162)", () => {
  test("docker, network_policy absent: not applicable — no banner, no ring", async ({ page }) => {
    await mockHealthz(page, { runner: "docker" });
    await gotoConsole(page);
    await expect(page.getByRole("status").filter({ hasText: NO_BANNER })).toHaveCount(0);

    const chip = firstConfinementChip(page);
    await expect(chip).toBeVisible();
    await expect(chip.locator("svg")).toHaveCount(1); // barrier icon only, no warning glyph
    const cls = (await chip.getAttribute("class")) ?? "";
    expect(cls).not.toContain("color-mix");
  });

  test("k8s, enforced: no banner, no ring — the canary proved it", async ({ page }) => {
    await mockHealthz(page, { runner: "k8s", network_policy: "enforced" });
    await gotoConsole(page);
    await expect(page.getByRole("status").filter({ hasText: NO_BANNER })).toHaveCount(0);

    const chip = firstConfinementChip(page);
    await expect(chip).toBeVisible();
    await expect(chip.locator("svg")).toHaveCount(1);
    const cls = (await chip.getAttribute("class")) ?? "";
    expect(cls).not.toContain("color-mix");
  });

  test("k8s, acknowledged: warning strip + soft ring, no glyph", async ({ page }) => {
    await mockHealthz(page, { runner: "k8s", network_policy: "acknowledged" });
    await gotoConsole(page);
    await expect(
      page.getByRole("status").filter({ hasText: "Network confinement is acknowledged, not proven." }),
    ).toBeVisible();
    await expect(page.getByRole("button", { name: "How to prove it" })).toBeVisible();

    const chip = firstConfinementChip(page);
    await expect(chip).toBeVisible();
    // Ring, not a second glyph — ruling 1 named the unenforced chip only.
    await expect(chip.locator("svg")).toHaveCount(1);
    const cls = (await chip.getAttribute("class")) ?? "";
    expect(cls).toMatch(/color-mix\(in_oklab,var\(--warning\)_28%/);
    expect(await chip.getAttribute("title")).toContain("network confinement is acknowledged, not proven");
  });

  // The strip's action opens the Admin view's Environment step. Plain /setup is
  // the User view's Getting Started (M-6), which has no such step.
  test("the strip's action opens the Admin view's Environment step", async ({ page }) => {
    // The funnel shows the welcome hero first until this flag is set.
    await page.addInitScript(() => {
      try {
        localStorage.setItem("wardyn-onboarding-seen", "1");
      } catch {
        /* private mode — ignore */
      }
    });
    await mockHealthz(page, { runner: "k8s", network_policy: "acknowledged" });
    await gotoConsole(page);
    await page.getByRole("button", { name: "How to prove it" }).click();

    await expect(page).toHaveURL(/\/admin\/setup\?step=environment$/);
    await expect(page.getByRole("heading", { name: "Pick your barrier", level: 2 })).toBeVisible();
  });

  test("k8s, unenforced: danger strip + warning ring + glyph", async ({ page }) => {
    await mockHealthz(page, { runner: "k8s", network_policy: "unenforced" });
    await gotoConsole(page);
    await expect(
      page.getByRole("status").filter({ hasText: "Runs on this cluster are not network-confined." }),
    ).toBeVisible();
    await expect(page.getByRole("button", { name: "What to fix" })).toBeVisible();

    const chip = firstConfinementChip(page);
    await expect(chip).toBeVisible();
    // Ruling 1: a glyph as well as the ring — colour is never the only signal.
    await expect(chip.locator("svg")).toHaveCount(2);
    const cls = (await chip.getAttribute("class")) ?? "";
    expect(cls).toMatch(/color-mix\(in_oklab,var\(--warning\)_55%/);
    expect(await chip.getAttribute("title")).toContain("network confinement is not enforced on this cluster");
    // D4 / the chip's own canon: the visible label stays Fence/Wall/Vault —
    // never the wire code — even while it is warning about this cluster.
    await expect(chip).toHaveText(/^(Fence|Wall|Vault)$/);
  });

  test("k8s, network_policy absent: could not confirm — warns, ruling 2, no chip ring", async ({ page }) => {
    await mockHealthz(page, { runner: "k8s" }); // network_policy key entirely absent
    await gotoConsole(page);
    await expect(
      page
        .getByRole("status")
        .filter({ hasText: "Wardyn can't confirm network confinement on this cluster." }),
    ).toBeVisible();
    await expect(page.getByRole("button", { name: "Where to look" })).toBeVisible();

    // Unknown/skew is a shell-level warning only — the mock never gave it a
    // chip treatment (no ring, no glyph), so the chip stays exactly as an
    // enforced/docker one would render.
    const chip = firstConfinementChip(page);
    await expect(chip).toBeVisible();
    await expect(chip.locator("svg")).toHaveCount(1);
    const cls = (await chip.getAttribute("class")) ?? "";
    expect(cls).not.toContain("color-mix");
  });
});
