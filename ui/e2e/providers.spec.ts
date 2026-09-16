/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import {
  test,
  expect,
  ADMIN_TOKEN,
  gotoConsole,
  mockMemberRole,
  mockSecurityAdminRole,
  navToRoute,
} from "./fixtures";
import { AGENTS, PROVIDERS, PROVIDERS_DRAFT } from "../src/app/lib/workspace-providers-copy";
import { OPERATOR_ONLY_REASON } from "../src/app/components/wardyn/copy";
import type { Page } from "@playwright/test";

// ---------------------------------------------------------------------------
// Workspace providers e2e (0.7.2) — lane: providers, port 8088, db wardyn_e2e.
//
// Every expected string is IMPORTED from lib/workspace-providers-copy.ts
// (§7.2-§7.5), never retyped — a copy change must break this spec rather than
// let the screen drift away from docs/design/workspace-providers-prompt.md.
//
// BROWSER VS API, and why (the drives.spec.ts precedent, `drives.spec.ts:66-
// 82`): the authoring walk below is driven for REAL — GET/PUT
// /workspace-providers are both operatorOnly routes, reachable with the
// seeded admin bearer, so this file writes to Postgres and reads its own
// writes back after a real reload. What it does NOT and CANNOT prove:
//   1. SERVER-SIDE admission (whether a repository actually clones or is
//      refused) — that is A3's, pinned in Go (the admission table tests).
//   2. TIER authorization for the security-admin/member negatives below —
//      mockSecurityAdminRole/mockMemberRole (fixtures.ts) splice /me's role
//      fields ONLY; this harness's bearer token is always admin server-side
//      (isOperator has no per-human session to demote), so these prove the
//      screen's RENDER behavior for that tier, never that GET/PUT
//      /workspace-providers actually refuse it — that is the operatorOnly
//      route group, authz_test.go's route matrix.
//   3. The malformed-URL write refusal below IS real: the 400 body rendered
//      is the server's own PROVIDERS_400.BASE_URL constant
//      (internal/api/workspace_providers.go), never a client-authored string.
// ---------------------------------------------------------------------------

const auth = { Authorization: `Bearer ${ADMIN_TOKEN}` };

// §9.1 Q2: /providers gets NO Workspaces-header button of its own — that
// slot already holds the drives button, and the plan is explicit "no second
// button". Its two entry points are the funnel step and the Settings card
// (drives-screen.tsx's own precedent), so this reaches it the same way the
// funnel/Settings-card test below already does, via the card's own link.
async function gotoProviders(page: Page): Promise<void> {
  await gotoConsole(page);
  await navToRoute(page, "/settings");
  const card = page.getByTestId("providers-card");
  await expect(card).toBeVisible();
  await card.getByText(PROVIDERS.CARD_OPEN).click();
  await expect(page).toHaveURL(/\/providers$/);
  await expect(page.getByRole("heading", { name: PROVIDERS.TITLE, level: 1 })).toBeVisible();
}

// Press Save and WAIT FOR THE WRITE, the way the admin does: the console's own
// success toast, which providers-screen.tsx raises only once the PUT has
// resolved 200 (the server writes inside the request, so a 200 IS a committed
// document).
//
// This is a write BARRIER, and the walk below needs one. `expect(...).
// toHaveCount(0)` on an error that has not happened yet is satisfied on its
// FIRST poll — it proves nothing about the in-flight PUT — so a wire read or a
// reload placed straight after the click raced the save: the ADO test read the
// PRE-save document in 4 of 20 repeats on an otherwise idle box (evidence:
// local/v073/evidence/fix-providers-flake/probe-baseline-nodelay.log, where
// the snapshot GET is logged ~26ms BEFORE the PUT's own 200). The reload in the
// GitHub test is the same race with a worse failure mode — navigating away
// ABORTS the in-flight PUT, and wardynd cancels the write with the request
// context. The toast closes both.
async function saveProviders(page: Page): Promise<void> {
  await page.getByRole("button", { name: PROVIDERS.SAVE_CTA }).click();
  await expect(page.getByText(PROVIDERS.SAVED_TOAST)).toBeVisible();
  // Now meaningful — the save has SETTLED, so these say "it settled without a
  // refusal", not merely "no refusal has rendered yet".
  await expect(page.getByText(PROVIDERS.SAVE_ERROR)).toHaveCount(0);
  await expect(page.getByText(PROVIDERS.SAVE_REFUSED_TITLE)).toHaveCount(0);
}

// Reset the document to a known-empty state before the authoring walk, so
// this file's own writes never depend on execution order or leftover state
// from a prior run of the same suite against a not-quite-fresh backend.
async function resetProviders(page: Page): Promise<void> {
  const res = await page.request.get("/api/v1/workspace-providers", { headers: auth });
  const etag = res.headers()["etag"] ?? null;
  const headers: Record<string, string> = { ...auth };
  if (etag) headers["If-Match"] = etag;
  await page.request.put("/api/v1/workspace-providers", { headers, data: {} });
}

test.describe.configure({ mode: "serial" });

test.describe("providers — legacy open mode, with no rows at all", () => {
  test("the empty registry is the legacy-open banner, and Settings/the funnel card say so too", async ({
    page,
  }) => {
    await resetProviders(page);
    await gotoProviders(page);

    await expect(page.getByText(PROVIDERS.LEGACY_OPEN_TITLE)).toBeVisible();
    await expect(page.getByText(PROVIDERS.LEGACY_OPEN_BODY)).toBeVisible();
    await expect(page.getByText(PROVIDERS.LEGACY_OPEN_OTHER_HOSTS)).toBeVisible();
    // The screen's one Save is withheld while the legacy banner alone is the
    // affirmative (providers-screen.tsx) — Add provider is the state's own
    // one teal.
    await expect(page.getByRole("button", { name: PROVIDERS.SAVE_CTA })).toHaveCount(0);

    // Settings card: zero enabled rows reads CARD_EMPTY, never a bare "0".
    await navToRoute(page, "/settings");
    const card = page.getByTestId("providers-card");
    await expect(card).toBeVisible();
    await expect(card.getByText(PROVIDERS.CARD_LEAD)).toBeVisible();
    await expect(card.getByText(PROVIDERS.CARD_EMPTY)).toBeVisible();
  });
});

test.describe("providers — the admin authoring walk (real writes, real reload)", () => {
  test.describe.configure({ mode: "serial" });

  test("enabling GitHub with a base URL persists across a reload", async ({ page }) => {
    await gotoProviders(page);
    await page.getByRole("button", { name: PROVIDERS.ADD_ROW_CTA }).click();

    const row = page.getByTestId("provider-row-github");
    await expect(row).toBeVisible();
    await row.locator("textarea").fill("https://github.com/acme");
    await saveProviders(page);

    await page.reload();
    await expect(page.getByRole("heading", { name: PROVIDERS.TITLE, level: 1 })).toBeVisible();
    const reloaded = page.getByTestId("provider-row-github");
    await expect(reloaded).toBeVisible();
    await expect(reloaded.locator("textarea")).toHaveValue("https://github.com/acme");

    // The wire itself — the strongest leg this harness can prove.
    const snap = await (await page.request.get("/api/v1/workspace-providers", { headers: auth })).json();
    expect(snap.git).toEqual([
      expect.objectContaining({ kind: "github", base_urls: ["https://github.com/acme"] }),
    ]);
  });

  test("enabling Azure DevOps shows `app` disabled with its own reason", async ({ page }) => {
    await gotoProviders(page);
    // The github row already exists (previous test) as a FULL row — it
    // renders no "Add provider" button of its own once populated (git-tab.tsx:
    // that CTA belongs only to the absent-row state). So with the ADO row
    // still absent, this is the one "Add provider" button left on the
    // screen — never re-adding github.
    await page.getByRole("button", { name: PROVIDERS.ADD_ROW_CTA }).click();

    const row = page.getByTestId("provider-row-azure_devops");
    await expect(row).toBeVisible();
    // `app` is offered WITH its reason, never hidden (Q3) — the checkbox is
    // named by LANE_META.app.label via its wrapping <label>, so the reason
    // line and the disabled state are both pinned to the SAME lane's control.
    const appCheckbox = row.getByRole("checkbox", { name: "App · brokered" });
    await expect(appCheckbox).toBeDisabled();
    await expect(row.getByText(PROVIDERS.LANE_APP_UNAVAILABLE)).toBeVisible();

    // FINDING (git-tab.tsx's addRow default, not fixed here — see this
    // lane's report): a freshly-added Azure DevOps row defaults its base URL
    // to "https://dev.azure.com" — ZERO path segments — which the server
    // refuses outright (workspace_providers.go: "dev.azure.com is shared by
    // every org on the planet, so the organization segment is REQUIRED
    // there"). Saving the row exactly as "Add provider" leaves it is a
    // guaranteed 400, with no client-side hint that the default itself is
    // unsavable. This test's job is the app-lane gate, not the default, so
    // it supplies a real org path before saving — the way an admin who hit
    // the refusal above would.
    await row.locator("textarea").fill("https://dev.azure.com/acme");
    await saveProviders(page);

    const snap = await (await page.request.get("/api/v1/workspace-providers", { headers: auth })).json();
    expect(snap.git.map((g: { kind: string }) => g.kind).sort()).toEqual(["azure_devops", "github"]);
  });

  test("storing a PAT inside the GitHub row writes the secret, inline", async ({ page }) => {
    await gotoProviders(page);
    const row = page.getByTestId("provider-row-github");
    await expect(row).toBeVisible();

    // The PAT lane is the row's default selection (no GitHub App configured,
    // no SSH key stored yet — git-tab.tsx's credLane default).
    await row.getByLabel("Access token").fill("ghp_e2e0000000000000000000000000000");
    await row.getByRole("button", { name: "Save", exact: true }).click();
    await expect(row.getByText(/Stored as/)).toBeVisible();
    await expect(row.getByText("git-pat-github-com")).toBeVisible();

    // A real secret write — GET /setup/status reflects it after a reload.
    await page.reload();
    await expect(page.getByTestId("provider-row-github").getByText(/Stored as/)).toBeVisible();
  });

  test("a malformed base URL is flagged before any request, and a server-only refusal renders the SERVER's own PROVIDERS_400 body — writing nothing", async ({
    page,
  }) => {
    await gotoProviders(page);
    const row = page.getByTestId("provider-row-github");
    const before = await (await page.request.get("/api/v1/workspace-providers", { headers: auth })).json();

    // The client mirror (providers/display.tsx baseURLError) flags the shape
    // the server would 400, and the screen offers no enabled Save while a row
    // is invalid — the request is never made.
    await row.locator("textarea").fill("not-a-url");
    await expect(row.getByText(PROVIDERS.BASE_URL_INVALID)).toBeVisible();
    await expect(page.getByRole("button", { name: PROVIDERS.SAVE_CTA, disabled: false })).toHaveCount(0);

    // A rule the mirror does not carry — the address COUNT — reaches the
    // server, whose own bytes (internal/api/workspace_providers.go's
    // providers400BaseURLNone) render under SAVE_REFUSED_TITLE, never a
    // console paraphrase. Nine valid GHES hosts pass every client rule.
    const nine = Array.from({ length: 9 }, (_, i) => `https://git${i + 1}.corp.example`).join("\n");
    await row.locator("textarea").fill(nine);
    await expect(row.getByText(PROVIDERS.BASE_URL_INVALID)).toHaveCount(0);
    await page.getByRole("button", { name: PROVIDERS.SAVE_CTA }).click();

    await expect(page.getByText(PROVIDERS.SAVE_REFUSED_TITLE)).toBeVisible();
    await expect(page.getByText("git[0].base_urls: name at least one address (at most 8)")).toBeVisible();

    // Nothing was written: the stored document is byte-identical to before.
    const after = await (await page.request.get("/api/v1/workspace-providers", { headers: auth })).json();
    expect(after.git).toEqual(before.git);
  });

  // R-2 (blind review, LOW): F4-F3 keeps the draft MOUNTED on a 412 — the
  // banner sits ABOVE the tabs rather than replacing them, with ONE control
  // ("Discard mine and reload", never "Save over theirs"). No spec pinned
  // this in a real browser before this pass.
  test("a 412 keeps the edited textarea on screen, with exactly one banner control", async ({ page }) => {
    await gotoProviders(page);
    const row = page.getByTestId("provider-row-github");
    await expect(row).toBeVisible();
    await row.locator("textarea").fill("https://github.com/acme\nhttps://git.corp.example/team");

    await page.route("**/api/v1/workspace-providers", async (route) => {
      if (route.request().method() !== "PUT") return route.fallback();
      await route.fulfill({
        status: 412,
        contentType: "application/json",
        body: JSON.stringify({ error: "providers changed since you loaded them — reload and retry" }),
      });
    });
    await page.getByRole("button", { name: PROVIDERS.SAVE_CTA }).click();

    await expect(page.getByText(PROVIDERS.SAVED_ELSEWHERE_TITLE)).toBeVisible();
    // The draft is still mounted and readable — the edited line survives.
    await expect(row.locator("textarea")).toHaveValue("https://github.com/acme\nhttps://git.corp.example/team");
    // ONE control on the banner: Discard mine and reload. No "Save over
    // theirs" — the corrected verdict refuses a second re-PUT arm.
    await expect(page.getByRole("button", { name: PROVIDERS_DRAFT.DISCARD_AND_RELOAD })).toBeVisible();
    await expect(page.getByText(/save over theirs/i)).toHaveCount(0);
    // Save providers is STILL on screen — the draft is still there to save.
    await expect(page.getByRole("button", { name: PROVIDERS.SAVE_CTA })).toBeVisible();

    // Clean up the route intercept and the in-memory draft edit before the
    // next serial test reads the real, unmodified stored document.
    await page.unroute("**/api/v1/workspace-providers");
    await page.reload();
    const reloaded = page.getByTestId("provider-row-github");
    await expect(reloaded).toBeVisible();
    await expect(reloaded.locator("textarea")).toHaveValue("https://github.com/acme");
  });

  test("the funnel step badge and Settings card both read the real enabled-provider count", async ({ page }) => {
    const snap = await (await page.request.get("/api/v1/workspace-providers", { headers: auth })).json();
    const enabledCount = (snap.git ?? []).filter((g: { disabled?: boolean }) => !g.disabled).length;
    expect(enabledCount).toBeGreaterThan(0);

    await gotoConsole(page);
    await navToRoute(page, "/setup");
    const stepBtn = page.getByRole("button", { name: new RegExp(`^${PROVIDERS.STEP_LABEL}`) });
    await expect(stepBtn).toBeVisible();
    await expect(stepBtn).toContainText(PROVIDERS.STEP_BADGE_READY(enabledCount));

    await navToRoute(page, "/settings");
    const card = page.getByTestId("providers-card");
    await expect(card).toBeVisible();
    await expect(card.getByText(PROVIDERS.CARD_PROVIDERS(enabledCount))).toBeVisible();
    await expect(card.getByText(PROVIDERS.CARD_EMPTY)).toHaveCount(0);
  });
});

test.describe("providers — the door is SUPER's alone", () => {
  test("a security admin sees the tier refusal — OperatorOnlyHint, and no form at all", async ({ page }) => {
    await mockSecurityAdminRole(page);
    await gotoConsole(page);

    // The Settings card itself is operator-gated (ProvidersCard returns null
    // for !operator) — a security admin's OWN authority over providers is
    // nothing (unlike drives, there is no governance-profile door for this
    // registry), so neither of the two entry points offers it.
    await navToRoute(page, "/settings");
    await expect(page.getByTestId("providers-card")).toHaveCount(0);

    // Reaching /providers directly: GET is operatorOnly, so a real security
    // admin's read genuinely answers 403 — routed here as the real server's
    // shape (the role splice leaves the bearer admin, exactly as
    // drives.spec.ts's equivalent test documents).
    await page.route("**/api/v1/workspace-providers", async (route) => {
      if (route.request().method() !== "GET") return route.fallback();
      await route.fulfill({ status: 403, contentType: "application/json", body: JSON.stringify({ error: "forbidden" }) });
    });
    await navToRoute(page, "/providers");
    await expect(page.getByRole("heading", { name: PROVIDERS.TITLE, level: 1 })).toBeVisible();
    await expect(page.getByText(OPERATOR_ONLY_REASON)).toBeVisible();
    // No form of any kind — no tabs, no Save, no rows.
    await expect(page.getByRole("button", { name: PROVIDERS.SAVE_CTA })).toHaveCount(0);
    await expect(page.getByTestId("provider-row-github")).toHaveCount(0);
    await expect(page.getByRole("button", { name: PROVIDERS.GIT_TITLE })).toHaveCount(0);
  });

  test("a member sees no nav, no card, and a 403 door if they reach the URL directly", async ({ page }) => {
    await mockMemberRole(page);
    await gotoConsole(page);

    // A member has no Settings screen at all (MEMBER_NAV_PATHS never lists
    // it, app-shell.tsx), so there is no card to check there — the negative
    // worth pinning is the route itself.
    await page.route("**/api/v1/workspace-providers", async (route) => {
      if (route.request().method() !== "GET") return route.fallback();
      await route.fulfill({ status: 403, contentType: "application/json", body: JSON.stringify({ error: "forbidden" }) });
    });
    await navToRoute(page, "/providers");
    await expect(page.getByRole("heading", { name: PROVIDERS.TITLE, level: 1 })).toBeVisible();
    await expect(page.getByText(OPERATOR_ONLY_REASON)).toBeVisible();
    await expect(page.getByTestId("provider-row-github")).toHaveCount(0);
  });
});

// Appendix A findings 2 + 5 (console-login lane, 0.7.3) — the Settings Model
// provider card's AWS Bedrock lane under a per_user roster row. Spliced onto
// GET /setup/status (this harness has no real per-user AWS SSO session to
// produce live/expired_signin for real — same reasoning as agents.spec.ts's
// model_access CASES loop). R8 (fix-first review pass): model_access carries
// `mechanism` + `action` alongside `state`, like the real payload always does
// for the states this helper is actually called with (live and the three
// actionable ones — see MODEL_ACCESS_ACTIONABLE) — the badge does not read
// either, but the fixture stays honest for whoever extends this. R-03
// (review): `not_applicable` is the one OTHER state modelAccessAction's own
// `default:` arm answers with "" (internal/api/modelaccess.go), matching
// `live` — not the fixture's prior blanket "every non-live state".
async function spliceBedrockRow(
  page: Page,
  credentialSource: "per_user" | "shared",
  modelAccessState: string | null,
): Promise<void> {
  await page.route("**/api/v1/setup/status*", async (route) => {
    // /setup/status is POLLED by this screen, so a handler can still be mid
    // `route.fetch()` when the test ends and the context tears down — Playwright
    // then disposes the response under it ("Response has been disposed") and the
    // throw is reported as the test's own failure. A tear-down race is not a
    // splice failure: let the request through and let the context close.
    let response: Awaited<ReturnType<typeof route.fetch>>;
    let json: Record<string, unknown>;
    try {
      response = await route.fetch();
      json = (await response.json()) as Record<string, unknown>;
    } catch {
      await route.fallback().catch(() => {});
      return;
    }
    const harnesses: Array<Record<string, unknown>> = Array.isArray(json.harnesses) ? json.harnesses : [];
    const idx = harnesses.findIndex((h) => h.id === "claude-code");
    const row = {
      ...(idx >= 0 ? harnesses[idx] : { id: "claude-code" }),
      enabled: true,
      mechanism: "bedrock_sso",
      credential_source: credentialSource,
    };
    if (idx >= 0) harnesses[idx] = row;
    else harnesses.push(row);
    json.harnesses = harnesses;
    if (modelAccessState) {
      json.model_access = {
        state: modelAccessState,
        mechanism: "bedrock_sso",
        action: modelAccessState === "live" || modelAccessState === "not_applicable" ? "" : "Sign in to AWS",
      };
    }
    await route.fulfill({ response, json }).catch(() => {});
  });
}

async function splicePerUserBedrock(page: Page, modelAccessState: string | null): Promise<void> {
  await spliceBedrockRow(page, "per_user", modelAccessState);
}

test.describe("providers — Settings Model provider card under a per_user Bedrock row", () => {
  // F5: the badge follows the CALLER's own model_access, not merely whether
  // the deployment has a Bedrock lane at all.
  test("Connected for a live caller", async ({ page }) => {
    await splicePerUserBedrock(page, "live");
    await gotoConsole(page);
    await navToRoute(page, "/settings");
    await expect(page.locator("#lane-bedrock")).toContainText("Connected");
  });

  test("not-Connected + Sign in with SSO for an expired caller", async ({ page }) => {
    await splicePerUserBedrock(page, "expired_signin");
    await gotoConsole(page);
    await navToRoute(page, "/settings");
    const bedrockLane = page.locator("#lane-bedrock");
    await expect(bedrockLane).not.toContainText("Connected");
    await bedrockLane.click();
    await expect(page.getByRole("button", { name: "Sign in with SSO" })).toBeVisible();
  });

  // U-02: not_applicable (the shared admin-token principal's own answer)
  // must NOT borrow the deployment-wide "Connected" badge either — it gets
  // its own honest, never-connected render with a neutral detail line.
  // Verbatim string, not imported: connection-cards.tsx pulls in
  // HarnessLoginPane -> AttachTerminal -> xterm's CSS, which Playwright's
  // Node-side spec collection cannot import (unlike the pure-data copy
  // modules other e2e specs import from).
  test("not_applicable: NOT connected, no borrowed badge — neutral per-person detail instead", async ({ page }) => {
    await splicePerUserBedrock(page, "not_applicable");
    await gotoConsole(page);
    await navToRoute(page, "/settings");
    const bedrockLane = page.locator("#lane-bedrock");
    await expect(bedrockLane).not.toContainText("Connected");
    await bedrockLane.click();
    await expect(
      page.getByText(
        "Per person — this caller is a mechanism, not a person, so it has no sign-in of its own. Each person's own AWS session carries their runs.",
      ),
    ).toBeVisible();
  });

  // U2-03 (blind round 2, lens-U2): the badge was honest and the door beside
  // it was not. `disabled={!operator}` gates nothing here — the admin token IS
  // operator — so the button was live, and the POST behind it is refused 422
  // (harnessLoginMechanismPrincipalRefusal). A door that cannot open must not
  // be on screen.
  test("not_applicable: the Sign in with SSO door is ABSENT, not merely disabled", async ({ page }) => {
    await splicePerUserBedrock(page, "not_applicable");
    await gotoConsole(page);
    await navToRoute(page, "/settings");
    await page.locator("#lane-bedrock").click();
    await expect(page.getByRole("button", { name: "Sign in with SSO" })).toHaveCount(0);
  });

  // F2: the server throws away a typed start URL under a per_user row
  // (harnesscred.go:761) — the dialog must never ask for one here.
  test("opening the dialog shows the managed-portal note, never the dead start-URL prompt", async ({ page }) => {
    await splicePerUserBedrock(page, "live");
    await gotoConsole(page);
    await navToRoute(page, "/settings");
    await page.locator("#lane-bedrock").click();
    await page.getByRole("button", { name: "Sign in with SSO" }).click();
    await expect(page.getByRole("dialog")).toBeVisible();
    await expect(page.getByText(AGENTS.SSO_START_URL_MANAGED)).toBeVisible();
    await expect(page.getByTestId("login-start-url-prompt")).toHaveCount(0);
  });

  // Unspliced negative control: the ordinary Settings sign-in (no per_user
  // row) is UNCHANGED — it still asks for the org's access portal URL.
  test("unspliced control: the ordinary Settings sign-in still prompts for the start URL", async ({ page }) => {
    await gotoConsole(page);
    await navToRoute(page, "/settings");
    // U2-01 (blind round 2, lens-U2): this daemon is the finding's own
    // reproduction — scripts/e2e-backend.sh sets WARDYN_BEDROCK_REGION and
    // WARDYN_BEDROCK_MODEL and NO credential of any kind (no bearer key, no
    // SSO session, no host ~/.aws mount, no static keys). Region + model used
    // to be enough to paint a green Connected chip over that, and this walk
    // went straight past the badge without looking. It looks now.
    await expect(page.locator("#lane-bedrock")).not.toContainText("Connected");
    await page.locator("#lane-bedrock").click();
    await page.getByRole("button", { name: "Sign in with SSO" }).click();
    await expect(page.getByTestId("login-start-url-prompt")).toBeVisible();
  });

  // P5 (0.7.3 field report): POST /setup/harness-login answers with the run id
  // BEFORE the sandbox is up now, so "resolved" no longer means "attachable" —
  // the pane holds a `starting` phase and keeps the id, which is what makes
  // Cancel able to kill a sandbox still coming up. Before this, a launch that
  // outran the console's 60s deadline left an orphan nobody could name.
  //
  // Spliced because this daemon runs `-runner none` (scripts/e2e-backend.sh)
  // and can never reach RUNNING; what is real is the pane's own machine.
  test("the sign-in pane narrates the wait, and Cancel kills a sandbox still coming up", async ({ page }) => {
    const loginRunId = "3f1b7c26-0000-4000-8000-00000000f002";
    await splicePerUserBedrock(page, "live");
    await page.route("**/api/v1/setup/harness-login", async (route) =>
      route.fulfill({ json: { run_id: loginRunId, state: "PENDING" } }),
    );
    await page.route(`**/api/v1/runs/${loginRunId}`, async (route) =>
      route.fulfill({ json: { id: loginRunId, task: "harness login", state: "PENDING", interactive: true } }),
    );
    let kills = 0;
    await page.route(`**/api/v1/runs/${loginRunId}/kill`, async (route) => {
      kills++;
      await route.fulfill({ status: 202, json: {} });
    });

    await gotoConsole(page);
    await navToRoute(page, "/settings");
    await page.locator("#lane-bedrock").click();
    await page.getByRole("button", { name: "Sign in with SSO" }).click();
    await page.getByRole("button", { name: /start login/i }).click();

    const starting = page.getByTestId("login-sandbox-starting");
    await expect(starting).toBeVisible();
    await expect(starting).toContainText("Starting the sign-in sandbox");
    await starting.getByRole("button", { name: /cancel/i }).click();
    await expect.poll(() => kills, { timeout: 10_000 }).toBeGreaterThanOrEqual(1);
  });

  // R3 (fix-first review pass): a spliced control BESIDE the unspliced one —
  // an EXPLICIT shared row (credential_source: "shared") behaves exactly
  // like no row at all, so the credential_source conjunct has a real e2e
  // negative rather than only the absent-field case above.
  test("spliced control: an explicit shared row also still prompts for the start URL", async ({ page }) => {
    await spliceBedrockRow(page, "shared", null);
    await gotoConsole(page);
    await navToRoute(page, "/settings");
    await page.locator("#lane-bedrock").click();
    await page.getByRole("button", { name: "Sign in with SSO" }).click();
    await expect(page.getByTestId("login-start-url-prompt")).toBeVisible();
  });
});
