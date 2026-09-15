/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { Page } from "@playwright/test";
import { test, expect, mockMemberRole } from "./fixtures";

// The setup gate, exercised as a USER experiences it — routed, rendered,
// clicked. Every case here was found manually on a live multi-user walk before
// any test covered it (the suite's own backend seeds its install as onboarded
// precisely to BYPASS this gate, which is why the gate needs its own spec: the
// bypass seam creates the obligation).
//
// The gated state is forced by intercepting /setup/status: onboarding_complete
// off, plus one warn-grade check. The interception rides on the REAL response,
// so every other field stays exactly what the daemon serves.

async function mockGatedStatus(
  page: Page,
  overrides: { onboarded?: boolean; sso?: boolean } = {},
): Promise<void> {
  await page.route("**/api/v1/setup/status*", async (route) => {
    const response = await route.fetch();
    const json = await response.json();
    json.onboarding_complete = overrides.onboarded ?? false;
    if (overrides.sso) {
      // The owner's live reproduction was the multi-user (SSO) funnel; sso
      // mode is also what renders People's "Open Permissions" affordance.
      json.auth = { ...json.auth, mode: "sso" };
    }
    json.checks = [
      ...(json.checks ?? []),
      {
        id: "e2e_gate_probe",
        label: "e2e gate probe",
        status: "warn",
        detail: "forced by setup-gate.spec.ts",
      },
    ];
    await route.fulfill({ response, json });
  });
}

// Seed the hero-seen flag so funnel tests land in the STEP funnel, not the
// welcome hero — the hero has its own spec; this one is about the gate.
async function skipHero(page: Page): Promise<void> {
  await page.addInitScript(() => {
    try {
      localStorage.setItem("wardyn-onboarding-seen", "1");
    } catch {
      /* private mode — ignore */
    }
  });
}

async function openPermissionsFromPeople(page: Page): Promise<void> {
  // The rail's steps are buttons; "Open Permissions" is a Link (role=link).
  await page.getByRole("button", { name: /^People/ }).click();
  await expect(page.getByRole("heading", { name: "Who can sign in" })).toBeVisible();
  await page.getByRole("link", { name: "Open Permissions" }).click();
}

test.describe("setup gate — forced on access, never a prison", () => {
  test.afterEach(async ({ page }) => {
    // The console polls setup/status; a poll in flight at teardown otherwise
    // surfaces as an orphan "route.fetch: Test ended" error.
    await page.unrouteAll({ behavior: "ignoreErrors" });
  });

  test("a fresh access to a gated install force-lands in Getting Started", async ({ page }) => {
    await mockGatedStatus(page);
    await page.goto("/");
    await page.waitForURL(/\/setup/);
    await expect(page.getByText("Getting started").first()).toBeVisible();
  });

  test("the funnel can leave itself: People → Open Permissions lands on /permissions", async ({
    page,
  }) => {
    // The exact bug the owner hit twice: the gate re-fired on every client-side
    // navigation, bouncing the funnel's own affordances back to step one.
    await mockGatedStatus(page, { sso: true });
    await skipHero(page);
    await page.goto("/");
    await page.waitForURL(/\/setup/);
    await openPermissionsFromPeople(page);
    await page.waitForURL(/\/permissions/);
    await expect(page).toHaveURL(/\/permissions/);
  });

  test("a cold load DIRECTLY on /setup still lets the funnel leave (the wrapper-unmount hole)", async ({
    page,
  }) => {
    // /setup sits outside the gate's route wrapper, so a load starting there
    // never rendered the wrapper — the once-per-load flag stayed unarmed and
    // the first navigation out re-fired the gate. Landing in the funnel now
    // arms it.
    await mockGatedStatus(page, { sso: true });
    await skipHero(page);
    await page.goto("/setup");
    await expect(page.getByText("Getting started").first()).toBeVisible();
    await openPermissionsFromPeople(page);
    await page.waitForURL(/\/permissions/);
    await expect(page).toHaveURL(/\/permissions/);
  });

  test("a NEW document load re-arms the gate — once per load, not once per browser", async ({
    page,
  }) => {
    await mockGatedStatus(page, { sso: true });
    await skipHero(page);
    await page.goto("/");
    await page.waitForURL(/\/setup/);
    await openPermissionsFromPeople(page);
    await page.waitForURL(/\/permissions/);
    // A full navigation (F5 / new tab) resets module state: the next ACCESS is
    // forced into the funnel again. This is the owner's requirement — every
    // site access lands a gated install in Getting Started.
    await page.goto("/runs");
    await page.waitForURL(/\/setup/);
  });

  test("negative control: an ONBOARDED install with the same warn is never gated", async ({
    page,
  }) => {
    // "A place you go, not a wall you are trapped behind": once the install
    // has been through onboarding, a degraded check informs, never confiscates.
    await mockGatedStatus(page, { onboarded: true });
    await page.goto("/runs");
    await expect(page).toHaveURL(/\/runs/);
    await expect(
      page.getByText(/each confined behind its own barrier/i),
    ).toBeVisible();
  });

  test("negative control: a member is never gated — their checks are redacted", async ({
    page,
  }) => {
    await mockGatedStatus(page);
    await mockMemberRole(page);
    await page.goto("/runs");
    await expect(page).toHaveURL(/\/runs/);
  });

  // Re-check means "look at the HOST again", and only the daemon can do that:
  // the host-proxy sweep it re-runs is memoized for 30s behind /setup/status
  // (internal/api/hostproxy_cache.go), so a button that merely refetched
  // returned a byte-identical answer and a proxy configured moments ago could
  // not be made to appear. ?recheck=1 is how the press says so, and it is the
  // ONLY request that carries it — the mount fetch and every background refresh
  // must not make the daemon sweep the host on a timer.
  test("Re-check asks the daemon to look again: the press carries recheck=1, the mount fetch does not", async ({
    page,
  }) => {
    await mockGatedStatus(page, { sso: true });
    await skipHero(page);

    const seen: string[] = [];
    page.on("request", (r) => {
      if (r.url().includes("/api/v1/setup/status")) seen.push(r.url());
    });

    await page.goto("/setup");
    await expect(page.getByText("Getting started").first()).toBeVisible();
    expect(
      seen.some((u) => u.includes("recheck=1")),
      `the mount fetch must not force a host sweep; saw ${seen.join(", ")}`,
    ).toBe(false);

    const pressed = page.waitForRequest((r) =>
      r.url().includes("/api/v1/setup/status") && r.url().includes("recheck=1"),
    );
    await page.getByRole("button", { name: "Re-check" }).first().click();
    await pressed;
  });

  // U2-06 (blind round 2, lens-U2): the test above pins a URL, and a URL is
  // not the complaint. The complaint was that pressing Re-check did not change
  // the ANSWER — hostProxyForceRedetect() zeroed the memo's timestamp but kept
  // its VALUE, so the forcing request was still served the old detection and
  // the operator had to press twice. This drives the answer itself: the
  // daemon's reply to the forced read carries the newly-configured proxy, and
  // the host-proxy evidence row must show it.
  //
  // Keyed on `recheck=1` rather than a request counter deliberately: the
  // counter version passes vacuously if anything else on the screen happens to
  // read /setup/status first, and "the forced read is the one that carries the
  // fresh sweep" IS the server contract this asserts against.
  test("Re-check changes the ANSWER: the host-proxy row shows the newly-configured proxy", async ({
    page,
  }) => {
    const PROXY = "http://proxy.e2e.invalid:3128";
    await page.route("**/api/v1/setup/status*", async (route) => {
      const response = await route.fetch();
      const json = await response.json();
      json.onboarding_complete = false;
      json.host_proxy = route.request().url().includes("recheck=1")
        ? { has_credentials: false, http_proxy: { value: PROXY, source: "env", has_credentials: false } }
        : { has_credentials: false };
      await route.fulfill({ response, json });
    });
    await skipHero(page);
    await page.goto("/setup?step=corp_network");

    const evidence = page.getByText(PROXY);
    await expect(page.getByRole("button", { name: "Re-check" }).first()).toBeVisible();
    await expect(evidence).toHaveCount(0);

    await page.getByRole("button", { name: "Re-check" }).first().click();
    await expect(evidence.first()).toBeVisible();
  });
});
