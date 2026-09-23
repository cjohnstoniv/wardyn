/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { Page } from "@playwright/test";
import { test, expect, mockMemberRole } from "./fixtures";

/**
 * Count the /setup/status reads the page has actually made. The console
 * coalesces a refocus that arrives while a read is in flight, so "I dispatched
 * an event" and "the app read the status" are different facts, and a test that
 * conflates them can pass because nothing happened.
 */
function countReads(page: Page): () => Promise<number> {
  let n = 0;
  page.on("response", (r) => {
    if (r.url().includes("/api/v1/setup/status")) n += 1;
  });
  return async () => n;
}

/**
 * Dispatch visibilitychange until `done` holds. One nudge is not enough: the
 * poll's in-flight guard drops a refocus that lands during a read, and clears
 * that guard a tick after the response, so whether any single dispatch causes a
 * read depends on timing this test does not control.
 */
async function nudgeUntil(page: Page, done: () => Promise<boolean>, timeoutMs = 20_000) {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    await page.evaluate(() => document.dispatchEvent(new Event("visibilitychange")));
    for (let i = 0; i < 8; i++) {
      if (await done()) return;
      await page.waitForTimeout(125);
    }
    if (Date.now() > deadline) throw new Error("the console never answered the refocus nudge");
  }
}

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
  overrides: { onboarded?: boolean; sso?: boolean; nonBlockingFail?: boolean } = {},
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
    // The e2e backend runs `-runner none` (scripts/e2e-backend.sh), so the REAL
    // runner row is a `fail` — and since 0.7.8 a runner fail is BLOCKING. Left
    // alone it would gate every case here on its own, so the probe below would
    // prove nothing and the not-gated cases could never load a page at all.
    // Clearing the flag on the real rows is what makes the probe the only thing
    // this spec gates on.
    json.checks = [
      ...(json.checks ?? []).map((c: { blocking?: boolean }) => ({ ...c, blocking: false })),
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
      // #161: a non-blocking fail, for the Review-step grouping case below —
      // it must never land under "Blocking" alongside the probe.
      ...(overrides.nonBlockingFail
        ? [{ id: "e2e_nonblocking_fail", label: "e2e non-blocking fail", status: "fail", detail: "forced by setup-gate.spec.ts" }]
        : []),
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
  // The rail itself is the real signal that the funnel's lazy chunk has
  // mounted — waited on explicitly rather than assumed, since the caller's
  // waitForURL(/\/setup/) resolves on the client-side route change alone and
  // can land well before the chunk (and the SSO-mode /access read the People
  // step's multi-user branch kicks off) are done. Without this the People
  // click below is the first thing to notice the rail isn't there yet, which
  // reads as "the button never appeared" rather than "the funnel is still
  // loading".
  await expect(page.getByRole("navigation", { name: "Setup steps" })).toBeVisible();
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
      // Same reason as mockGatedStatus: the backend's own `-runner none` fail
      // row is blocking, and would gate the landing read this case needs to
      // get PAST before it can prove anything about a background refresh.
      json.checks = (json.checks ?? []).map((c: { blocking?: boolean }) => ({ ...c, blocking: false }));
      // The row the owner's own admin carried — graded through THEIR credential,
      // not the install's. Warn, and never blocking: present from the first read
      // so that staying on New Run below is a statement about `blocking`, not
      // about the row being absent.
      json.checks.push({
        id: "harness_credential_aws",
        label: "AWS SSO session",
        status: "warn",
        detail: "your AWS SSO session has expired",
      });
      if (blocking) {
        json.checks.push({
          id: "e2e_gate_probe",
          label: "e2e gate probe",
          status: "warn",
          blocking: true,
        });
      }
      await route.fulfill({ response, json });
    });

    await page.goto("/runs/new");
    await expect(page.getByRole("heading", { name: "New run" })).toBeVisible();

    // usePoll drops a refocus while a read is already in flight
    // (`if (pausedRef.current || inFlight.current) return`), and it clears that
    // flag a tick AFTER the response lands. So a single dispatch is not a
    // guarantee of a read, and a waiter registered around one dispatch can
    // match a read that some earlier nudge started. Both halves below therefore
    // nudge until the app has actually answered, rather than assuming one
    // dispatch produces one read.
    const reads = countReads(page);

    // FIRST, the fix itself: a warn the daemon did not mark blocking — the very
    // row that used to throw this admin onto Getting started — survives a
    // background refresh with the person still on New Run. The read has to be
    // observed, or "still on New Run" would also be true of a nudge that was
    // swallowed and never read anything at all.
    await nudgeUntil(page, async () => (await reads()) >= 1);
    await expect(page).toHaveURL(/\/runs\/new$/);

    // THEN the positive: the install now fails a genuinely blocking check — as
    // it would seconds after the owner clicked Launch — and the NEXT read gates.
    blocking = true;
    await nudgeUntil(page, async () => /\/setup/.test(page.url()));
    await expect(page).toHaveURL(/\/setup/);
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

// #213 — the step counter counts only what blocks a run, the rail keeps three
// categories apart (Required / Optional setup / Demos, not one flat
// "optional" list), and the barrier recommendation is derived from what the
// host reports installed, never inferred from hardware or the OS.
test.describe("setup counter and rail — three categories, not two (#213)", () => {
  test.afterEach(async ({ page }) => {
    await page.unrouteAll({ behavior: "ignoreErrors" });
  });

  test("Environment shows the four-step counter and its honest, live-derived subline", async ({ page }) => {
    await mockGatedStatus(page);
    await skipHero(page);
    await page.goto("/");
    await page.waitForURL(/\/setup/);
    await expect(page.getByRole("heading", { name: /pick your barrier/i })).toBeVisible();
    await expect(page.getByText("Step 1 of 4")).toBeVisible();
    // "3" (CONFIG_STEPS) is a constant; the demo count is derived live from
    // stepOrder(status), so it's asserted by pattern, not a hand-kept number.
    await expect(
      page.getByText(/^Required before a run can launch\. 3 optional setup steps and \d+ demos follow\.$/),
    ).toBeVisible();
  });

  // #161: the Review step partitions on `blocking` first, not on grade — a
  // blocking warn must not read as optional, and a non-blocking fail must not
  // read as a wall. mockGatedStatus's probe (warn, blocking: true) plus the
  // nonBlockingFail addition (fail, no blocking) prove both sides at once.
  test("Review groups a blocking warn under Blocking, and a non-blocking fail under Worth a look", async ({
    page,
  }) => {
    await mockGatedStatus(page, { nonBlockingFail: true });
    await skipHero(page);
    await page.goto("/");
    await page.waitForURL(/\/setup/);
    // Review sits behind the Corporate network gate (steps.ts: "no click-past
    // corp_network"), so it has to be cleared first — same proof the rail test
    // above uses (the e2e backend's `-runner none` answers `no_runner`, which
    // clears the gate on this host).
    const rail = page.getByRole("navigation", { name: /setup steps/i }).last();
    await rail.getByRole("button", { name: /^Network/ }).click();
    await page.getByRole("button", { name: /^Test connectivity$/i }).click();
    await expect(page.getByRole("button", { name: /^Next:/i })).toBeEnabled();
    await rail.getByRole("button", { name: /^Review/ }).click();
    await expect(page.getByRole("heading", { name: /review readiness/i })).toBeVisible();
    const blocking = page.locator("section").filter({ has: page.getByText("Blocking", { exact: true }) });
    await expect(blocking.getByText("e2e gate probe")).toBeVisible();
    const worthALook = page.locator("section").filter({ has: page.getByText("Worth a look", { exact: true }) });
    await expect(worthALook.getByText("e2e non-blocking fail")).toBeVisible();
    await expect(blocking.getByText("e2e non-blocking fail")).toHaveCount(0);
  });

  test("the rail keeps Required / Optional setup / Demos apart, and Secrets (not required) is still reachable with the optional-step footer", async ({
    page,
  }) => {
    await mockGatedStatus(page);
    await skipHero(page);
    await page.goto("/");
    await page.waitForURL(/\/setup/);
    await expect(page.getByRole("heading", { name: /pick your barrier/i })).toBeVisible();

    // Scoped to the full rail's own landmark: "Required" is also a substring
    // of the step-counter's subline ("Required before a run can launch…"),
    // and Playwright's text matcher is substring/case-insensitive by default.
    const navs = page.getByRole("navigation", { name: /setup steps/i });
    const rail = navs.last();
    await expect(rail.getByText("Required", { exact: true })).toBeVisible();
    await expect(rail.getByText("· 4")).toBeVisible();
    await expect(rail.getByText("Optional setup")).toBeVisible();
    await expect(rail.getByText("· 3")).toBeVisible();
    await expect(rail.getByText("Demos", { exact: true })).toBeVisible();

    // Prove the mandatory Network gate (the e2e backend has no real sandbox
    // runner, so the probe answers no_runner — the one honest bypass) before
    // Secrets becomes reachable: it is real configuration, not one of the
    // four required steps, but it still sits BEHIND the same crossing gate.
    await rail.getByRole("button", { name: /^Network/ }).click();
    await expect(page.getByRole("heading", { name: "Network" })).toBeVisible();
    await page.getByRole("button", { name: /^Test connectivity$/i }).click();
    await expect(page.getByRole("button", { name: /^Next:/i })).toBeEnabled();

    // Secrets opens from the rail, and its footer is the optional-step pair,
    // never a numbered Next.
    await rail.getByRole("button", { name: /^Secrets/ }).click();
    await expect(page.getByRole("heading", { name: "Secrets" })).toBeVisible();
    await expect(page.getByRole("button", { name: /^Next:/ })).toHaveCount(0);
    await expect(page.getByRole("button", { name: "Back to required steps" })).toBeVisible();
    await expect(page.getByRole("button", { name: "Done with this one" })).toBeVisible();
  });

  test("a host reporting no barrier gets no Recommended chip, and the honest note names why", async ({
    page,
  }) => {
    await page.route("**/api/v1/setup/status*", async (route) => {
      const response = await route.fetch();
      const json = await response.json();
      json.onboarding_complete = false;
      json.runner = { driver: "docker", confinement_classes: [] };
      await route.fulfill({ response, json });
    });
    await skipHero(page);
    await page.goto("/setup");
    await expect(page.getByRole("heading", { name: /pick your barrier/i })).toBeVisible();
    // exact: Playwright's default text match is substring + case-insensitive,
    // and the honest note below contains "recommended" as a lowercase word.
    await expect(page.getByText("Recommended", { exact: true })).toHaveCount(0);
    await expect(
      page.getByText(
        "Nothing is recommended while the host reports no barrier. Wardyn recommends what it can see, not what the operating system suggests.",
      ),
    ).toBeVisible();
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
