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
        // 0.7.8: setupGateActive gates on `blocking`, not on grade/id — a
        // probe with no flag would silently stop testing anything this spec
        // exists to cover.
        blocking: true,
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

  // The path the owner actually walked (0.7.6): the gate fires once per PAGE
  // LOAD (gateFiredThisLoad in setup-gate.ts), so a live goto() only ever
  // exercises the LANDING read — every other case above lands already gated.
  // What sent the owner's admin to Getting started mid-session was a
  // BACKGROUND refresh (App.tsx's status poll) handing back a newly-blocking
  // check while they sat on New Run. usePoll refetches on tab refocus
  // ("coming back to the tab refreshes now" — its own comment) rather than
  // waiting out its 5-minute interval, so that refocus tick — not a timer —
  // is what this case drives, by dispatching the same `visibilitychange`
  // event the real return-to-tab does.
  test("a status that turns blocking on a BACKGROUND refresh gates too, not just the landing read", async ({
    page,
  }) => {
    let blocking = false;
    await page.route("**/api/v1/setup/status*", async (route) => {
      const response = await route.fetch();
      const json = await response.json();
      json.onboarding_complete = false;
      if (blocking) {
        json.checks = [
          ...(json.checks ?? []),
          { id: "e2e_gate_probe", label: "e2e gate probe", status: "warn", blocking: true },
        ];
      }
      await route.fulfill({ response, json });
    });

    await page.goto("/runs/new");
    await expect(page.getByRole("heading", { name: "New run" })).toBeVisible();

    // The install now fails a blocking check — as it would seconds after the
    // owner clicked Launch — and the NEXT read must carry it.
    blocking = true;
    await page.evaluate(() => document.dispatchEvent(new Event("visibilitychange")));
    await page.waitForURL(/\/setup/);
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
    // R-09: the payload MOVED, so the strip may say so.
    await expect(page.getByText("Checked just now")).toBeVisible();
  });

  // R-09 (fix-s2 review): the other half. hostProxyRecheck waits only
  // hostProxyRecheckWait (2s) for the sweep it started and then answers with
  // LAST-KNOWN — so against a wedged host the forced read returns the very
  // payload the poll would have returned, and nothing on the wire says which
  // of the two happened (no checked_at, no stale flag). Pressing the button is
  // therefore not proof the host was looked at, and the strip must not say it
  // was. Same route, same press, payload held CONSTANT.
  test("a forced Re-check that changes nothing claims no fresh check (the wedged host)", async ({
    page,
  }) => {
    await page.route("**/api/v1/setup/status*", async (route) => {
      const response = await route.fetch();
      const json = await response.json();
      json.onboarding_complete = false;
      json.host_proxy = { has_credentials: false }; // identical on every read, forced or not
      await route.fulfill({ response, json });
    });
    await skipHero(page);
    await page.goto("/setup?step=corp_network");

    const recheck = page.getByRole("button", { name: "Re-check" }).first();
    await expect(recheck).toBeVisible();
    // Armed BEFORE the click — a waiter created afterwards misses the request
    // it is waiting for and simply times out.
    const pressed = page.waitForRequest((r) =>
      r.url().includes("/api/v1/setup/status") && r.url().includes("recheck=1"),
    );
    await recheck.click();
    await pressed; // the press landed — the daemon was asked, and answered
    // …and the strip still claims nothing about when the host was last seen.
    await expect(page.getByText(/^Checked /)).toHaveCount(0);
    await expect(page.getByText(/^Last checked /)).toHaveCount(0);
  });
});

// F3-F8/F7-F7: the egress-redirect "From" picker (FromCombobox) opens a
// PopoverContent fixed at w-[420px] — wider than a 390px viewport, which used
// to force horizontal scroll on the WHOLE page the moment it opened, not just
// clip the popover. ui/popover.tsx's primitive-level
// max-w-[calc(100vw-2rem)] (this lane) is what keeps it inside the viewport.
test.describe("egress-redirect endpoint picker at 390px (F3-F8/F7-F7)", () => {
  test("390px: opening the From picker does not force horizontal scroll", async ({ page }) => {
    await page.setViewportSize({ width: 390, height: 844 });
    await skipHero(page);
    await page.goto("/setup?step=corp_network");
    await page.getByRole("tab", { name: /Egress redirection/ }).click();

    // Baseline, not an absolute zero-overflow assertion: F7-2 (the shell
    // header's own 390px responsiveness, ui-setup-shell's item) is a
    // SEPARATE finding this lane does not own, so this test isolates what
    // THIS lane's popover fix controls — the picker must not make an
    // existing scrollWidth WORSE — rather than asserting a repo-wide
    // invariant this lane can't singlehandedly guarantee.
    const scrollWidth = () => page.evaluate(() => document.documentElement.scrollWidth);
    const before = await scrollWidth();

    const picker = page.getByRole("combobox").filter({ hasText: /https:\/\/…, host, or IP/ });
    await expect(picker).toBeVisible();
    await picker.click();
    await expect(page.getByPlaceholder("https://…, host, or IP").last()).toBeVisible();

    // The defect: the popover's own fixed width (w-[420px], wider than the
    // 390px viewport) used to push page scrollWidth wider still the instant
    // it opened. max-w-[calc(100vw-2rem)] keeps it from adding any.
    await expect.poll(scrollWidth, "scrollWidth after opening the picker").toBeLessThanOrEqual(before);
  });
});
