/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { Page } from "@playwright/test";
import { test, expect } from "./fixtures";
import { EPISODES } from "../src/app/lib/demo-videos";

// The welcome hero's episode catalog, grouped by deployment path (Shape C,
// approved mock round 2026-08-31). The hero only renders on a
// not-yet-onboarded install, and the harness deliberately seeds itself
// onboarded — so the un-onboarded state is forced the same way
// setup-gate.spec.ts forces it: ride the real /setup/status response and flip
// what the scenario needs (the bypass-seam rule: the seam that skips a
// behavior obligates the spec that covers it).

async function mockFreshInstall(page: Page, opts: { sso?: boolean } = {}): Promise<void> {
  // CACHE-AND-SERVE (fixtures.ts#mockMemberSetupStatus): one real fetch,
  // retried, and every match served from that body, so App's status is the
  // same SSO body for the whole test. The hero renders from App's status
  // (onboarding-screen.tsx), and a request log shows ONE /setup/status read.
  // This does NOT explain the SSO case's CI flake (#469): there, the whole
  // /setup page never paints within the timeout while the retry paints in
  // under a second, which points at the lazy Getting-started route, not at
  // this mock. setup-gate.spec fails the same way.
  let cached: Record<string, unknown> | null = null;
  await page.route("**/api/v1/setup/status*", async (route) => {
    let lastErr: unknown;
    for (let attempt = 0; attempt < 3 && !cached; attempt++) {
      try {
        const json = (await (await route.fetch()).json()) as Record<string, unknown>;
        json.onboarding_complete = false;
        if (opts.sso) json.auth = { ...(json.auth as object), mode: "sso" };
        cached = json;
      } catch (e) {
        lastErr = e;
      }
    }
    if (!cached) throw lastErr;
    await route.fulfill({ json: cached });
  });
}

test.describe("episode catalog — Shape C path grouping", () => {
  test.afterEach(async ({ page }) => {
    await page.unrouteAll({ behavior: "ignoreErrors" });
  });

  test("single-user install: core leads, single-user path is the deployment group, multi collapses", async ({
    page,
  }) => {
    await mockFreshInstall(page);
    await page.goto("/");
    await page.waitForURL(/\/setup/);
    await expect(page.getByText("All episodes")).toBeVisible();
    await expect(page.getByText("Start here")).toBeVisible();
    await expect(page.getByText("Your deployment — single-user")).toBeVisible();
    await expect(page.getByText("Running work — any deployment")).toBeVisible();
    await expect(page.getByText("Your deployment — multi-user")).toHaveCount(0);
    // The other path is reachable behind a disclosure, with an honest count.
    const disclosure = page.getByText(/^The multi-user path — \d+ episodes$/);
    await expect(disclosure).toBeVisible();
    await disclosure.click();
    await expect(page.getByText("One command to a cluster")).toBeVisible();
  });

  test("multi-user (SSO) install: the deployment group swaps and member rows are chipped", async ({
    page,
  }) => {
    await mockFreshInstall(page, { sso: true });
    await page.goto("/");
    await page.waitForURL(/\/setup/);
    // CI-flake: waitForURL resolves on the client-side route change alone, not
    // on the funnel's lazy chunk finishing or the mocked-but-real /setup/status
    // round trip landing — both still outstanding at this point. The first
    // paint-dependent assertion after the navigate is what has to absorb that
    // on a loaded CI host; real slack via Playwright's own retry, not a sleep.
    await expect(page.getByText("Your deployment — multi-user")).toBeVisible({ timeout: 15_000 });
    await expect(page.getByText("Your deployment — single-user")).toHaveCount(0);
    await expect(page.getByText(/^The single-user path — \d+ episodes$/)).toBeVisible();
    // The multi path's member-audience episodes carry the chip. DERIVED from
    // the catalog, never a literal: this count was hard-coded 2 and episode
    // 04d ("Your drive", multi/member) made it 3, which only this spec could
    // see — `make ci` does not run Playwright, so the drift sat on the one
    // layer nothing else covers. EpisodeList chips exactly the multi group's
    // member rows (episode-card.tsx's `memberChips && e.audience === "member"`),
    // so the filter below IS the render rule, and the next episode cannot
    // break this the same way.
    const memberChips = EPISODES.filter((e) => e.path === "multi" && e.audience === "member").length;
    expect(memberChips, "the multi path has no member-audience episode to chip").toBeGreaterThan(0);
    await expect(page.getByText("For your members")).toHaveCount(memberChips);
  });
});

// #145: an operator-configured video mirror (WARDYN_DEMO_VIDEO_BASE_URL) swaps
// the catalog summary and the per-episode stream-note/load-error copy for
// their configured-source twins (canon docs/design/demo-video-source-canon.md)
// — and per Q145-2, the mirror's own host/URL must never render as text.
//
// This spec only mocks /healthz's demo_video_base_url (client-side) — the
// real CSP media-src header stays whatever this e2e backend actually booted
// with (no WARDYN_DEMO_VIDEO_BASE_URL here), which does NOT admit the fake
// mirror host below. So clicking Watch deterministically hits the browser's
// own CSP block, same as a real misconfigured/redirecting mirror would
// (canon's "fails CLOSED... no server error at all") — which is exactly the
// case LOAD_ERROR_CONFIGURED exists for, and lets this spec prove it end to
// end: the configured twin renders (not the default GitHub error, no
// release-page link), and the host never leaks into rendered text.
test.describe("episode catalog — configured video source (/healthz's demo_video_base_url)", () => {
  const MIRROR_HOST = "videos.airgapped.example";
  const MIRROR_BASE = `https://${MIRROR_HOST}/wardyn-demos`;

  test.afterEach(async ({ page }) => {
    await page.unrouteAll({ behavior: "ignoreErrors" });
  });

  test("a configured source shows the configured catalog summary and CSP-block copy, never the mirror host as text", async ({
    page,
  }) => {
    await mockFreshInstall(page);
    await page.route("**/healthz", async (route) => {
      const res = await route.fetch();
      const body = await res.json();
      body.demo_video_base_url = MIRROR_BASE;
      await route.fulfill({ response: res, json: body });
    });

    await page.goto("/");
    await page.waitForURL(/\/setup/);
    await expect(page.getByText("All episodes")).toBeVisible();
    await expect(
      page.getByText(/recorded · about \d+ minutes · streamed from your admin's video source on click/),
    ).toBeVisible();
    await expect(page.getByText(/streamed from GitHub on click/)).toHaveCount(0);

    // The first row with a Watch button is the first recorded (tag !== null)
    // core episode — "00" (Meet Wardyn) is unrecorded and shows "Not recorded
    // yet" instead, so this is "01" (Why govern agents).
    const watch = page.getByRole("button", { name: "Watch" }).first();
    await watch.click();
    await expect(
      page.getByText(
        "Couldn't play this episode. This deployment only allows video from the source your admin configured, and this didn't come from it.",
      ),
    ).toBeVisible();
    await expect(page.getByText(/^Couldn't load this episode from GitHub/)).toHaveCount(0);
    await expect(page.getByRole("link", { name: "Open the release page" })).toHaveCount(0);

    // Q145-2: the mirror host/URL never renders as visible text, on a
    // configured deployment's error state any more than its streaming one.
    const bodyText = await page.locator("body").innerText();
    expect(bodyText).not.toContain(MIRROR_HOST);
  });
});
