/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { Page } from "@playwright/test";
import { test, expect } from "./fixtures";
import { PEOPLE_STEP } from "../src/app/components/wardyn/copy";
import { ACCESS_ERROR, ACCESS_STATE, GUARD, PEOPLE, PREVIEW } from "../src/app/lib/people-access-copy";

// People step's role-mappings editor (0.7 SSO Phase 3, ui/src/app/components/
// screens/setup/access-panel.tsx). The seeded e2e backend runs with
// cfg.OIDC == nil (no real OIDC configured — see internal/api/access.go's
// requireOIDC), so GET/POST/DELETE /api/v1/access* always fail closed with a
// REAL 503 in this harness. That is honest and load-bearing for case (g)
// below, but every other case needs a shaped response no real backend call
// here can ever produce, so those intercept with page.route(...fulfill)
// rather than the route.fetch()+patch technique setup-gate.spec.ts uses (that
// technique only works when a REAL successful response exists to build on —
// /setup/status is the one endpoint here that has one).

async function mockSsoStatus(page: Page): Promise<void> {
  await page.route("**/api/v1/setup/status", async (route) => {
    const response = await route.fetch();
    const json = await response.json();
    // Only auth.mode changes — onboarding_complete stays whatever the real
    // seeded backend reports (true; e2e-backend.sh's cmd_seed marks it), so
    // this never accidentally exercises the setup GATE (that's
    // setup-gate.spec.ts's own job).
    json.auth = { ...json.auth, mode: "sso" };
    await route.fulfill({ response, json });
  });
}

function baseAccessBody(over: Record<string, unknown> = {}) {
  return {
    mappings: [],
    default_role: "",
    operator_emails_present: false,
    // W-4: the real server sends null (Go nil slice), not [] — the panel must
    // survive it. A fixture of [] here masked a crash the live walk exposed.
    operator_emails: null,
    allow_email_mappings: false,
    email_domains_configured: false,
    posture: { map_empty: true, before: "an admin", after: "be denied", changes: true },
    ...over,
  };
}

async function mockAccessGet(page: Page, body: unknown): Promise<void> {
  await page.route("**/api/v1/access", async (route) => {
    if (route.request().method() !== "GET") return route.fallback();
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(body) });
  });
}

async function gotoPeopleStep(page: Page): Promise<void> {
  await page.goto("/setup?step=people");
  await expect(page.getByRole("heading", { name: "Who can sign in" })).toBeVisible();
}

test.describe("People step — role mappings editor (0.7 SSO Phase 3)", () => {
  test.afterEach(async ({ page }) => {
    // Mirrors setup-gate.spec.ts: a poll/fetch in flight at teardown otherwise
    // surfaces as an orphan "route.fetch: Test ended" error.
    await page.unrouteAll({ behavior: "ignoreErrors" });
  });

  // ---------------------------------------------------------------------
  // (a) The four filmed anchors — demo/04c-who-may-do-what.spec.ts:56-63's
  // exact beats, pinned as a fast canary independent of any access-endpoint
  // mock (the heading/badge/link all come from status-derived, not
  // access-derived, state).
  // ---------------------------------------------------------------------
  test("(a) the four filmed anchors: heading, rail badge (not the step-body chip), Open Permissions", async ({ page }) => {
    await mockSsoStatus(page);
    await gotoPeopleStep(page);

    // "Who can sign in" is the ONLY heading matching this text anywhere on
    // the page.
    await expect(page.getByRole("heading", { name: /who can sign in/i })).toHaveCount(1);

    // "Multi-user" renders in THREE places: the compact icon-rail's sr-only
    // label ("People — Multi-user", always in the DOM but visually hidden —
    // phase-rail.tsx's lg-only/xl-hidden rail), the full rail's own badge
    // span, and the step body's MULTI_USER_CHIP — the exact same string
    // (wardyn/copy.ts's PEOPLE_STEP.MULTI_USER_CHIP). DOM order decides which
    // one .first() lands on (setup-layout.tsx renders the rail <aside>
    // before the content <div>, and PhaseRail renders the compact rail
    // before the full one), so ancestry — not text, and not visibility — is
    // what proves this is the rail's, not the body's.
    const firstMultiUser = page.getByText("Multi-user").first();
    const isInsideRail = await firstMultiUser.evaluate((el) => !!el.closest('[aria-label="Setup steps"]'));
    expect(isInsideRail).toBe(true);

    await expect(page.getByRole("link", { name: "Open Permissions" })).toBeVisible();
  });

  // ---------------------------------------------------------------------
  // (b) Editor renders: merged table with chart row (read-only) + console
  // rows; shadowed badge (both causes); Defaults with real addresses; the
  // email-row warn badge only when allow_email_mappings.
  // ---------------------------------------------------------------------
  test("(b) merged table: a read-only chart row, a deletable console row, and Defaults with real addresses", async ({ page }) => {
    await mockSsoStatus(page);
    await mockAccessGet(
      page,
      baseAccessBody({
        mappings: [
          { value: "Wardyn.Admin", role: "admin", source: "chart", shadowed: false, shadow_cause: "" },
          {
            id: "c1",
            value: "alice@corp.example",
            role: "admin",
            source: "console",
            shadowed: false,
            shadow_cause: "",
            created_at: "2026-08-28T00:00:00Z",
            created_by: "admin",
          },
        ],
        default_role: "member",
        operator_emails_present: true,
        operator_emails: ["ops@corp.example", "sre@corp.example"],
      }),
    );
    await gotoPeopleStep(page);

    await expect(page.getByText("Wardyn.Admin")).toBeVisible();
    await expect(page.getByText(PEOPLE.CHART_HINT)).toBeVisible();
    await expect(page.getByText("alice@corp.example")).toBeVisible();
    await expect(page.getByRole("button", { name: `${PEOPLE.DELETE} alice@corp.example` })).toBeVisible();
    // The chart row has no delete affordance at all.
    await expect(page.getByRole("button", { name: `${PEOPLE.DELETE} Wardyn.Admin` })).toHaveCount(0);

    const defaults = page.getByTestId("access-defaults");
    await expect(defaults.getByText(PEOPLE.ROLE_MEMBER)).toBeVisible();
    await expect(defaults.getByText("ops@corp.example")).toBeVisible();
    await expect(defaults.getByText("sre@corp.example")).toBeVisible();
  });

  test("(b) a shadowed row carries the right badge for both causes", async ({ page }) => {
    await mockSsoStatus(page);
    await mockAccessGet(
      page,
      baseAccessBody({
        mappings: [
          { value: "Wardyn.Contractors", role: "member", source: "chart", shadowed: false, shadow_cause: "" },
          {
            id: "c2",
            value: "Wardyn.Contractors",
            role: "member",
            source: "console",
            shadowed: true,
            shadow_cause: "chart",
            created_at: "2026-08-30T00:00:00Z",
          },
          {
            id: "c3",
            value: "carol@corp.example",
            role: "member",
            source: "console",
            shadowed: true,
            shadow_cause: "operator_allowlist",
            created_at: "2026-08-30T00:00:00Z",
          },
        ],
      }),
    );
    await gotoPeopleStep(page);

    await expect(page.getByText(PEOPLE.SHADOWED_BADGE)).toBeVisible();
    await expect(page.getByText(PEOPLE.SHADOWED_OPERATOR_BADGE)).toBeVisible();
  });

  test("(b) an email-shaped row gets no unverified-claim badge when email mappings are off", async ({ page }) => {
    await mockSsoStatus(page);
    await mockAccessGet(
      page,
      baseAccessBody({
        mappings: [
          {
            id: "c4",
            value: "bob@corp.example",
            role: "member",
            source: "console",
            shadowed: false,
            shadow_cause: "",
            created_at: "2026-08-30T00:00:00Z",
          },
        ],
        allow_email_mappings: false,
      }),
    );
    await gotoPeopleStep(page);

    await expect(page.getByText("bob@corp.example")).toBeVisible();
    await expect(page.getByText(PEOPLE.EMAIL_KEY_BADGE)).toHaveCount(0);
  });

  test("(b) the unverified-claim badge appears once email mappings are opted in", async ({ page }) => {
    await mockSsoStatus(page);
    await mockAccessGet(
      page,
      baseAccessBody({
        mappings: [
          {
            id: "c4",
            value: "bob@corp.example",
            role: "member",
            source: "console",
            shadowed: false,
            shadow_cause: "",
            created_at: "2026-08-30T00:00:00Z",
          },
        ],
        allow_email_mappings: true,
      }),
    );
    await gotoPeopleStep(page);

    await expect(page.getByText(PEOPLE.EMAIL_KEY_BADGE)).toBeVisible();
  });

  // ---------------------------------------------------------------------
  // (c) Add flow: a form submit POSTs; a mocked structured posture-flip 400
  // flips the dialog into guard mode with the parameterized consequence copy
  // visible IN-VIEWPORT; an acknowledged retry sends
  // acknowledge_access_change.
  // ---------------------------------------------------------------------
  test("(c) add: a reactive posture-flip 400 flips into guard mode in-viewport; the ack retry sends acknowledge_access_change", async ({
    page,
  }) => {
    await mockSsoStatus(page);
    // map_empty:false so onAddClick submits straight through (no client-side
    // pre-emptive guard) — the guard here comes purely from the server's 400,
    // proving the REACTIVE path, not the pre-check.
    await mockAccessGet(
      page,
      baseAccessBody({ posture: { map_empty: false, before: "an admin", after: "sign in as a member", changes: true } }),
    );

    const postCalls: Array<{ value: string; role: string; acknowledge_access_change?: boolean }> = [];
    await page.route("**/api/v1/access/mappings", async (route) => {
      if (route.request().method() !== "POST") return route.fallback();
      const body = route.request().postDataJSON() as {
        value: string;
        role: string;
        acknowledge_access_change?: boolean;
      };
      postCalls.push(body);
      if (!body.acknowledge_access_change) {
        await route.fulfill({
          status: 400,
          contentType: "application/json",
          body: JSON.stringify({
            error: 'this change moves every unmatched signed-in human from "an admin" to "sign in as a member"',
            required_acknowledgement: true,
            before: "an admin",
            after: "sign in as a member",
          }),
        });
      } else {
        await route.fulfill({
          status: 201,
          contentType: "application/json",
          body: JSON.stringify({ id: "new1", value: body.value, role: body.role, created_by: "e2e", created_at: new Date().toISOString() }),
        });
      }
    });

    await gotoPeopleStep(page);
    await page.getByLabel(PEOPLE.FIELD_VALUE, { exact: true }).fill("eng-team");
    await page.getByRole("button", { name: PEOPLE.ADD_CTA }).click();

    // The click issued a real POST — this is the "form submit POSTs" case.
    await expect.poll(() => postCalls.length).toBe(1);

    const dialog = page.getByRole("alertdialog");
    await expect(dialog).toBeVisible();
    await expect(dialog.getByText(GUARD.FIRST_ROW_TITLE)).toBeVisible();
    const body = dialog.getByText(GUARD.FIRST_ROW_BODY("an admin", "sign in as a member"));
    await expect(body).toBeInViewport();

    const confirm = dialog.getByRole("button", { name: GUARD.FIRST_ROW_CONFIRM });
    await expect(confirm).toBeDisabled();
    await page.getByLabel(GUARD.GUARD_ACK_LABEL).check();
    await expect(confirm).toBeEnabled();
    await confirm.click();

    await expect.poll(() => postCalls.length).toBe(2);
    expect(postCalls[1].acknowledge_access_change).toBe(true);
    await expect(page.getByRole("alertdialog")).toHaveCount(0);
  });

  // ---------------------------------------------------------------------
  // (d) Delete flow — server-authoritative: a structured 400 on DELETE flips
  // the OPEN dialog into the reverse-guard copy in-viewport; a plain delete
  // confirms otherwise.
  // ---------------------------------------------------------------------
  test("(d) delete: a structured 400 flips the open dialog into the reverse-guard copy in-viewport; ack retry sends the query param", async ({
    page,
  }) => {
    await mockSsoStatus(page);
    await mockAccessGet(
      page,
      baseAccessBody({
        mappings: [
          {
            id: "c1",
            value: "alice@corp.example",
            role: "admin",
            source: "console",
            shadowed: false,
            shadow_cause: "",
            created_at: "2026-08-28T00:00:00Z",
          },
        ],
        operator_emails_present: true,
      }),
    );

    const deleteCalls: string[] = [];
    await page.route("**/api/v1/access/mappings/*", async (route) => {
      if (route.request().method() !== "DELETE") return route.fallback();
      const url = route.request().url();
      deleteCalls.push(url);
      const acked = new URL(url).searchParams.get("acknowledge_access_change") === "true";
      if (!acked) {
        await route.fulfill({
          status: 400,
          contentType: "application/json",
          body: JSON.stringify({
            error: 'this change moves every unmatched signed-in human from "sign in as a member" to "an admin"',
            required_acknowledgement: true,
            before: "an admin",
            after: "sign in as a member",
          }),
        });
      } else {
        await route.fulfill({ status: 204, body: "" });
      }
    });

    await gotoPeopleStep(page);
    await page.getByRole("button", { name: `${PEOPLE.DELETE} alice@corp.example` }).click();

    // Plain confirm first — no ack checkbox — the panel never client-side
    // guesses "is this the last row" (access-panel.tsx's own doc).
    const dialog = page.getByRole("alertdialog");
    await expect(dialog.getByText('Delete the mapping for "alice@corp.example"?')).toBeVisible();
    await expect(dialog.getByText(GUARD.GUARD_ACK_LABEL)).toHaveCount(0);
    await dialog.getByRole("button", { name: PEOPLE.DELETE, exact: true }).click();

    await expect.poll(() => deleteCalls.length).toBe(1);

    // The SAME open dialog flips into the reverse-guard copy.
    await expect(dialog.getByText(GUARD.LAST_ROW_TITLE)).toBeVisible();
    const guardBody = dialog.getByText(GUARD.LAST_ROW_BODY("sign in as a member", "an admin"));
    await expect(guardBody).toBeInViewport();
    await expect(dialog.getByText(GUARD.ALLOWLIST_NOTE)).toBeVisible();

    const confirm = dialog.getByRole("button", { name: GUARD.LAST_ROW_CONFIRM });
    await expect(confirm).toBeDisabled();
    await page.getByLabel(GUARD.GUARD_ACK_LABEL).check();
    await confirm.click();

    await expect.poll(() => deleteCalls.length).toBe(2);
    expect(deleteCalls[1]).toContain("acknowledge_access_change=true");
    await expect(page.getByRole("alertdialog")).toHaveCount(0);
  });

  test("(d) delete: a plain delete (no posture flip) confirms directly", async ({ page }) => {
    await mockSsoStatus(page);
    await mockAccessGet(
      page,
      baseAccessBody({
        mappings: [
          {
            id: "c1",
            value: "alice@corp.example",
            role: "admin",
            source: "console",
            shadowed: false,
            shadow_cause: "",
            created_at: "2026-08-28T00:00:00Z",
          },
          {
            id: "c2",
            value: "eng-team",
            role: "member",
            source: "console",
            shadowed: false,
            shadow_cause: "",
            created_at: "2026-08-29T00:00:00Z",
          },
        ],
      }),
    );
    await page.route("**/api/v1/access/mappings/*", async (route) => {
      if (route.request().method() !== "DELETE") return route.fallback();
      await route.fulfill({ status: 204, body: "" });
    });

    await gotoPeopleStep(page);
    await page.getByRole("button", { name: `${PEOPLE.DELETE} alice@corp.example` }).click();
    const dialog = page.getByRole("alertdialog");
    await expect(dialog).toBeVisible();
    await dialog.getByRole("button", { name: PEOPLE.DELETE, exact: true }).click();
    await expect(page.getByRole("alertdialog")).toHaveCount(0);
  });

  // ---------------------------------------------------------------------
  // (e) Error arms — each rendered in-viewport, from its EXACT server
  // string/shape.
  // ---------------------------------------------------------------------
  async function submitAddExpectingError(page: Page, statusBody: { status: number; body: unknown }): Promise<void> {
    await mockSsoStatus(page);
    await mockAccessGet(page, baseAccessBody({ posture: { map_empty: false, before: "x", after: "y", changes: false } }));
    await page.route("**/api/v1/access/mappings", async (route) => {
      if (route.request().method() !== "POST") return route.fallback();
      await route.fulfill({ status: statusBody.status, contentType: "application/json", body: JSON.stringify(statusBody.body) });
    });
    await gotoPeopleStep(page);
    await page.getByLabel(PEOPLE.FIELD_VALUE, { exact: true }).fill("some-value");
    await page.getByRole("button", { name: PEOPLE.ADD_CTA }).click();
  }

  test("(e) collision cause=chart renders the frozen COLLISION_ERROR_CHART string, in-viewport", async ({ page }) => {
    await submitAddExpectingError(page, {
      status: 400,
      body: {
        error: 'value "wardyn.admin" is already set by your chart config (WARDYN_OIDC_ROLE_MAP) and cannot be overridden here',
        cause: "chart",
        value: "Wardyn.Admin",
      },
    });
    await expect(page.getByText(ACCESS_ERROR.COLLISION_ERROR_CHART("Wardyn.Admin"))).toBeInViewport();
  });

  test("(e) collision cause=operator_allowlist renders the DISTINCT COLLISION_ERROR_OPERATOR string, in-viewport", async ({ page }) => {
    await submitAddExpectingError(page, {
      status: 400,
      body: {
        error: 'value "ops@corp.example" is already set by your chart config (WARDYN_OIDC_OPERATOR_EMAILS) and cannot be overridden here',
        cause: "operator_allowlist",
        value: "ops@corp.example",
      },
    });
    await expect(page.getByText(ACCESS_ERROR.COLLISION_ERROR_OPERATOR("ops@corp.example"))).toBeInViewport();
    await expect(page.getByText(ACCESS_ERROR.COLLISION_ERROR_CHART("ops@corp.example"))).toHaveCount(0);
  });

  test("(e) a lockout 400 renders the frozen LOCKOUT_ERROR, in-viewport", async ({ page }) => {
    await submitAddExpectingError(page, {
      status: 400,
      body: { error: "this change would remove your own admin access (checked against your last sign-in)" },
    });
    await expect(page.getByText(ACCESS_ERROR.LOCKOUT_ERROR)).toBeInViewport();
    await expect(page.getByText(ACCESS_ERROR.STALE_SNAPSHOT_ERROR)).toHaveCount(0);
  });

  test("(e) a stale-snapshot 400 renders STALE_SNAPSHOT_ERROR, DISTINCT from lockout, in-viewport", async ({ page }) => {
    await submitAddExpectingError(page, {
      status: 400,
      body: { error: "your sign-in is too old to verify this change — sign in again before changing role mappings" },
    });
    await expect(page.getByText(ACCESS_ERROR.STALE_SNAPSHOT_ERROR)).toBeInViewport();
    await expect(page.getByText(ACCESS_ERROR.LOCKOUT_ERROR)).toHaveCount(0);
  });

  test("(e) EMAIL_KEY_REFUSED renders verbatim, in-viewport", async ({ page }) => {
    await submitAddExpectingError(page, {
      status: 400,
      body: { error: ACCESS_ERROR.EMAIL_KEY_REFUSED },
    });
    await expect(page.getByText(ACCESS_ERROR.EMAIL_KEY_REFUSED)).toBeInViewport();
  });

  // ---------------------------------------------------------------------
  // (f) Preview wiring: typed claims POST -> role+matched rendered (three
  // arms: matched / default / would-be-denied) + couldn't-check on
  // {error:"role_check_unavailable"}.
  // ---------------------------------------------------------------------
  test("(f) preview: matched, default, denied, and couldn't-check all render from the real POST response", async ({ page }) => {
    await mockSsoStatus(page);
    // map_empty:false — the DEFAULT arm (not the LEGACY one) is what's under
    // test here.
    await mockAccessGet(page, baseAccessBody({ posture: { map_empty: false, before: "x", after: "y", changes: false } }));

    let call = 0;
    await page.route("**/api/v1/access/preview", async (route) => {
      call++;
      const responses = [
        { role: "admin", ok: true, matched: [{ value: "Wardyn.Admin", role: "admin", source: "chart" }] },
        { role: "member", ok: true, matched: [] },
        { role: "", ok: false, matched: [] },
        { role: "", ok: false, matched: [], error: "role_check_unavailable" },
      ];
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(responses[call - 1]) });
    });

    await gotoPeopleStep(page);
    const claims = page.getByLabel(PREVIEW.FIELD_CLAIMS);
    const run = page.getByRole("button", { name: PREVIEW.RUN_CTA });

    await claims.fill("Wardyn.Admin");
    await run.click();
    await expect(page.getByText(PREVIEW.RESULT_MATCHED("admin", "Wardyn.Admin"))).toBeVisible();

    await claims.fill("nobody");
    await run.click();
    await expect(page.getByText(PREVIEW.RESULT_DEFAULT("member"))).toBeVisible();

    await claims.fill("nobody-else");
    await run.click();
    await expect(page.getByText(PREVIEW.RESULT_DENIED)).toBeVisible();

    await claims.fill("whoever");
    await run.click();
    await expect(page.getByText(PREVIEW.RESULT_UNKNOWN)).toBeVisible();
  });

  // ---------------------------------------------------------------------
  // (g) ONE non-intercepted case: /setup/status flipped to sso but /access
  // left REAL — the harness backend has no OIDC configured, so
  // requireOIDC's 503 renders the SSO-not-configured state. Pinned distinct
  // from an intercepted 500's fetch-failed state.
  // ---------------------------------------------------------------------
  test("(g) real 503 from the OIDC-less harness renders the SSO-not-configured state", async ({ page }) => {
    await mockSsoStatus(page);
    // NO /access route — this hits the real seeded backend, which runs with
    // cfg.OIDC == nil.
    await gotoPeopleStep(page);
    await expect(page.getByText(ACCESS_STATE.SSO_UNAVAILABLE_TITLE)).toBeVisible();
    await expect(page.getByText(ACCESS_STATE.SSO_UNAVAILABLE_BODY)).toBeVisible();
    await expect(page.getByText(ACCESS_STATE.FETCH_FAILED_TITLE)).toHaveCount(0);
  });

  test("(g) an intercepted 500 renders fetch-failed — distinct from the real 503's SSO-not-configured state", async ({ page }) => {
    await mockSsoStatus(page);
    await page.route("**/api/v1/access", async (route) => {
      if (route.request().method() !== "GET") return route.fallback();
      await route.fulfill({ status: 500, contentType: "application/json", body: JSON.stringify({ error: "boom" }) });
    });
    await gotoPeopleStep(page);
    await expect(page.getByText(ACCESS_STATE.FETCH_FAILED_TITLE)).toBeVisible();
    await expect(page.getByText(ACCESS_STATE.SSO_UNAVAILABLE_TITLE)).toHaveCount(0);
  });

  // ---------------------------------------------------------------------
  // (h) Single-user negative: without the sso flip, the People step shows
  // the unchanged explainer, no editor, and fires zero /access requests.
  // ---------------------------------------------------------------------
  test("(h) single-user: unchanged explainer, no editor mounted, and zero /access requests", async ({ page }) => {
    let accessHit = false;
    await page.route("**/api/v1/access", async (route) => {
      accessHit = true;
      await route.fulfill({ status: 200, contentType: "application/json", body: "{}" });
    });

    // No mockSsoStatus — the real seeded harness backend defaults to a
    // non-sso auth.mode (token, this backend's admin bearer), so
    // deploymentMode() reads single-user without any intercept.
    await gotoPeopleStep(page);

    // "Single-user" also renders in the rail (compact + full, same collision
    // as "Multi-user" above) — the step body's OWN chip is the LAST match in
    // DOM order (the rail <aside> precedes the content <div>).
    await expect(page.getByText(PEOPLE_STEP.SINGLE_USER_CHIP).last()).toBeVisible();
    await expect(page.getByText(PEOPLE_STEP.SINGLE_USER_BODY)).toBeVisible();
    // The editor (AccessPanel) never mounts on the single-user branch at all.
    await expect(page.getByText(PEOPLE.TABLE_TITLE)).toHaveCount(0);
    expect(accessHit).toBe(false);
  });
});
