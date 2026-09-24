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
  sidebarLink,
} from "./fixtures";
import { AGENTS, PROVIDERS, PROVIDERS_DRAFT } from "../src/app/lib/workspace-providers-copy";
import { AVAILABILITY } from "../src/app/lib/availability-copy";
import { OPERATOR_ONLY_REASON, UNSAVED_GUARD } from "../src/app/components/wardyn/copy";
import { LOGIN_SANDBOX_SLOW_START } from "../src/app/components/screens/settings/login-start-wait";
// U-15: the starting sentence is a constant in a CSS-free module now — this
// spec used to re-type its opening clause, so a reworded wait could move on
// screen while the assertion went on passing.
import { LOGIN_SANDBOX_STARTING } from "../src/app/components/screens/settings/login-pane-copy";
// auth-tab-handle.ts is a pure TS module (no React, no xterm.css) — safe for
// Playwright's Node-side spec collection, unlike the pane itself.
import { AUTH_TAB_BLOCKED_NOTE } from "../src/app/components/screens/settings/auth-tab-handle";
import type { BrowserContext, Page } from "@playwright/test";

// review-1 S3: the AWS device-authorization URL this spec's REAL-terminal
// cases navigate the auto-opened tab to — inside extractDeviceVerificationUrl's
// own host allowlist (harness-login-pane.tsx), unlike the kind walk's fake.
const DEVICE_VERIFICATION_URL = "https://device.sso.us-east-1.amazonaws.com/?user_code=ABCD-EFGH";

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
    // refuses outright (workspace_providers_baseurl.go: "dev.azure.com is shared by
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
    // The stored name appears twice in the row (the "Stored as" line AND the lane radio's
    // connectedDetail "host · stored as <name>"), so an unscoped getByText is a strict-mode
    // violation whenever both have rendered — a load-dependent flake. Assert the line itself.
    await expect(row.getByText(/Stored as/)).toContainText("git-pat-github-com");

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
  // banner sits ABOVE the tabs rather than replacing them. #217: the banner's
  // two controls are Copy my changes (first) and Discard mine and reload
  // (second, now ghost) — never a "Save over theirs" arm.
  test("a 412 keeps the edited textarea on screen; Copy my changes, then Discard", async ({ page, context }) => {
    // Chromium refuses navigator.clipboard.writeText without this — the
    // console's own useCopyToClipboard (lib/use-copy-to-clipboard.ts)
    // degrades to a failure toast otherwise, which is real browser behavior
    // this spec isn't testing.
    await context.grantPermissions(["clipboard-read", "clipboard-write"]);
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
    await expect(page.getByText(PROVIDERS.SAVED_ELSEWHERE_BODY)).toBeVisible();
    // The draft is still mounted and readable — the edited line survives.
    await expect(row.locator("textarea")).toHaveValue("https://github.com/acme\nhttps://git.corp.example/team");
    await expect(page.getByText(/save over theirs/i)).toHaveCount(0);
    // Save providers is STILL on screen — the draft is still there to save.
    await expect(page.getByRole("button", { name: PROVIDERS.SAVE_CTA })).toBeVisible();

    // #217 — the changed field, as readable text (never the whole draft as
    // JSON): the banner shows it before Copy is even pressed, then Copy
    // confirms with a toast once it is.
    await expect(page.getByText(/base_urls.*acme.*→.*acme.*corp\.example/s)).toBeVisible();
    await page.getByRole("button", { name: PROVIDERS_DRAFT.CONFLICT_COPY }).click();
    await expect(page.getByText(PROVIDERS_DRAFT.CONFLICT_COPIED_TOAST)).toBeVisible();

    // Discard mine and reload is still there, now beside Copy, not the only
    // exit.
    await expect(page.getByRole("button", { name: PROVIDERS_DRAFT.DISCARD_AND_RELOAD })).toBeVisible();

    // Clean up the route intercept and the in-memory draft edit before the
    // next serial test reads the real, unmodified stored document.
    await page.unroute("**/api/v1/workspace-providers");
    await page.reload();
    const reloaded = page.getByTestId("provider-row-github");
    await expect(reloaded).toBeVisible();
    await expect(reloaded.locator("textarea")).toHaveValue("https://github.com/acme");
  });

  // #217 — the guard is armed the moment the draft differs from what loaded,
  // and it is a BLOCKING confirm, not a banner: a sidebar click away from a
  // dirty Providers draft must stop and ask before it navigates.
  test("editing a field arms the unsaved-navigation guard; Keep editing stays, Discard changes leaves", async ({ page }) => {
    await gotoProviders(page);
    const row = page.getByTestId("provider-row-github");
    await expect(row).toBeVisible();
    await row.locator("textarea").fill("https://github.com/acme\nhttps://git.corp.example/team");
    // The dirty marker beside Save — the same fact the guard is armed on.
    await expect(page.getByText(PROVIDERS_DRAFT.UNSAVED_MARKER)).toBeVisible();

    await sidebarLink(page, "Settings").click();
    const dialog = page.getByRole("alertdialog");
    await expect(dialog).toBeVisible();
    await expect(dialog.getByText(UNSAVED_GUARD.TITLE)).toBeVisible();
    await expect(dialog.getByText(UNSAVED_GUARD.BODY)).toBeVisible();
    // The navigation did NOT happen — still on /providers.
    await expect(page).toHaveURL(/\/providers$/);

    // Keep editing: the dialog closes, the edit and the route both survive.
    await dialog.getByRole("button", { name: UNSAVED_GUARD.STAY }).click();
    await expect(dialog).toHaveCount(0);
    await expect(page).toHaveURL(/\/providers$/);
    await expect(row.locator("textarea")).toHaveValue("https://github.com/acme\nhttps://git.corp.example/team");

    // The same click, answered the other way, actually leaves.
    await sidebarLink(page, "Settings").click();
    await expect(page.getByRole("alertdialog")).toBeVisible();
    await page.getByRole("button", { name: UNSAVED_GUARD.LEAVE }).click();
    await expect(page).toHaveURL(/\/settings$/);

    // The unmounted draft never reached the server — the next test's read of
    // the stored document must see the ORIGINAL row, not this edit.
    await page.reload();
    await navToRoute(page, "/providers");
    const reloaded = page.getByTestId("provider-row-github");
    await expect(reloaded).toBeVisible();
    await expect(reloaded.locator("textarea")).toHaveValue("https://github.com/acme");
  });

  // #217 — a disabled control states its reason BESIDE it, never only in a
  // title tooltip. GET/PUT /workspace-providers are admin-only server-side
  // (requireOperator), so a real security admin's read would 403 before this
  // control ever paints — this spec's own documented ceiling (BROWSER VS API,
  // top of file) is what makes the state reachable at all: the harness's
  // bearer stays real admin, so the GET genuinely succeeds while the spliced
  // client role reads !operator, exactly the race a stale /me can produce.
  test("a security-admin session sees Save disabled WITH its reason beside it", async ({ page }) => {
    await mockSecurityAdminRole(page);
    // NOT gotoProviders(): the Settings card itself is operator-gated
    // (ProvidersCard returns null for !operator — the sibling describe
    // block's own test above), so that entry point is gone for this role.
    // The GET still succeeds for real (the harness's bearer stays admin),
    // so the route itself renders.
    await gotoConsole(page);
    await navToRoute(page, "/providers");
    await expect(page.getByRole("heading", { name: PROVIDERS.TITLE, level: 1 })).toBeVisible();
    const saveBtn = page.getByRole("button", { name: PROVIDERS.SAVE_CTA });
    await expect(saveBtn).toBeVisible();
    await expect(saveBtn).toBeDisabled();
    await expect(page.getByText(OPERATOR_ONLY_REASON)).toBeVisible();
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
  // #337: bedrock_bearer's twin call (below) is the ONLY caller that passes
  // this — every existing call keeps splicing bedrock_sso, unchanged.
  mechanism: "bedrock_sso" | "bedrock_bearer" = "bedrock_sso",
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
      mechanism,
      credential_source: credentialSource,
    };
    if (idx >= 0) harnesses[idx] = row;
    else harnesses.push(row);
    json.harnesses = harnesses;
    if (modelAccessState) {
      json.model_access = {
        state: modelAccessState,
        mechanism,
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
    await expect(starting).toContainText(LOGIN_SANDBOX_STARTING);
    await starting.getByRole("button", { name: /cancel/i }).click();
    await expect.poll(() => kills, { timeout: 10_000 }).toBeGreaterThanOrEqual(1);
  });

  // Finding 7a (0.7.5 field report, this lane's own spec): the verification
  // tab never opened because it was opened from a PTY callback, never a user
  // gesture. These cases pin the CLICK-side half — a page opens on the click
  // itself, needing no PTY output at all (`-runner none` keeps this daemon's
  // login run PENDING forever, so the PTY-dependent half — the tab actually
  // NAVIGATING to a real device-authorization URL, a repeated URL line, focus
  // restoration after a real capture — has no real terminal to drive it here
  // and is recorded as a gap in TEST-GAPS, not silently skipped).
  test.describe("the verification tab opens on the click (Finding 7a)", () => {
    async function openStartingPane(page: Page, runId: string): Promise<void> {
      await splicePerUserBedrock(page, "live");
      await page.route("**/api/v1/setup/harness-login", async (route) =>
        route.fulfill({ json: { run_id: runId, state: "PENDING" } }),
      );
      await page.route(`**/api/v1/runs/${runId}`, async (route) =>
        route.fulfill({ json: { id: runId, task: "harness login", state: "PENDING", interactive: true } }),
      );
      await gotoConsole(page);
      await navToRoute(page, "/settings");
      await page.locator("#lane-bedrock").click();
      await page.getByRole("button", { name: "Sign in with SSO" }).click();
    }

    // review-1 S3: a REAL terminal, on the `-runner none` backend. The run
    // read answers RUNNING (never PENDING) and the attach-ticket POST answers
    // a ticket (member-getting-started.spec.ts:228's own precedent), then
    // `page.routeWebSocket` stands in for the daemon's attach socket — the
    // SAME two frame shapes AttachTerminal itself reads (attach-terminal.tsx):
    // a text attach-mode frame, then a binary PTY chunk. This is what makes
    // the click-opened tab's NAVIGATE half (not just its open) provable here.
    async function attachWithRealTerminal(page: Page, context: BrowserContext, runId: string): Promise<void> {
      await splicePerUserBedrock(page, "live");
      await page.route("**/api/v1/setup/harness-login", async (route) =>
        route.fulfill({ json: { run_id: runId, state: "PENDING" } }),
      );
      await page.route(`**/api/v1/runs/${runId}`, async (route) =>
        route.fulfill({ json: { id: runId, task: "harness login", state: "RUNNING", interactive: true } }),
      );
      await page.route(`**/api/v1/runs/${runId}/attach-ticket`, async (route) =>
        route.fulfill({ json: { ticket: "e2e-ticket" } }),
      );
      await page.routeWebSocket(/\/api\/v1\/runs\/[^/]+\/attach/, (ws) => {
        ws.send(JSON.stringify({ type: "attach-mode", read_only: false }));
        ws.send(Buffer.from(`${DEVICE_VERIFICATION_URL}\n`));
      });
      // Context-level (not page-level) so the NEW tab the click opens is
      // covered too — the whole point of this stub.
      await context.route(`${DEVICE_VERIFICATION_URL.split("?")[0]}**`, (route) =>
        route.fulfill({ contentType: "text/html", body: "<title>stub</title>" }),
      );
      await gotoConsole(page);
      await navToRoute(page, "/settings");
      await page.locator("#lane-bedrock").click();
      await page.getByRole("button", { name: "Sign in with SSO" }).click();
    }

    test("clicking Start opens a page — before the launch POST even resolves", async ({ page, context }) => {
      await splicePerUserBedrock(page, "live");
      // The launch POST never resolves: if the tab-open were hoisted below an
      // `await`, this proves it by never opening at all.
      await page.route("**/api/v1/setup/harness-login", () => {});
      await gotoConsole(page);
      await navToRoute(page, "/settings");
      await page.locator("#lane-bedrock").click();
      await page.getByRole("button", { name: "Sign in with SSO" }).click();

      const pagePromise = context.waitForEvent("page");
      await page.getByRole("button", { name: /start login/i }).click();
      const tab = await pagePromise;
      await expect(tab).toHaveTitle("Wardyn — waiting for the sign-in page");
      await tab.close();
    });

    // review-1 S6 (rewritten — the original body never checked the link or
    // the note its own name claimed): a REAL terminal, so the pane actually
    // reaches `attached` with a verification URL on screen, and `tabBlocked`
    // is genuinely true (window.open blocked even on the click).
    test("a browser that blocks the popup still offers the header link, with a note explaining why", async ({
      page,
      context,
    }) => {
      const runId = "3f1b7c26-0000-4000-8000-00000000f011";
      // Simulate a strict popup policy: window.open answers null, exactly the
      // contract openAuthTab() already handles.
      await page.addInitScript(() => {
        window.open = () => null;
      });
      await attachWithRealTerminal(page, context, runId);
      await page.getByRole("button", { name: /start login/i }).click();

      await expect(page.getByTestId("auth-url-link")).toBeVisible();
      await expect(page.getByTestId("auth-url-link")).toHaveAttribute("href", DEVICE_VERIFICATION_URL);
      await expect(page.getByTestId("auth-tab-blocked-note")).toHaveText(AUTH_TAB_BLOCKED_NOTE);
    });

    // review-1 S6 (rewritten — the original body never clicked Cancel; it
    // closed the tab itself). "launching" renders no Cancel button at all
    // (nothing to click yet) — the real Cancel path a person can reach is
    // from "starting", once the POST has resolved; this is the case the
    // original name was reaching for.
    test("Cancel closes the tab it opened, once the sandbox is starting", async ({ page, context }) => {
      const runId = "3f1b7c26-0000-4000-8000-00000000f012";
      await openStartingPane(page, runId);

      const pagePromise = context.waitForEvent("page");
      await page.getByRole("button", { name: /start login/i }).click();
      const tab = await pagePromise;
      expect(tab.isClosed()).toBe(false);

      // expect.poll, not tab.waitForEvent("close") — the click's own JS
      // handler (cancel() -> closeAuthTab() -> tab.close()) can close the
      // tab SYNCHRONOUSLY inside the click, before a `waitForEvent` call
      // placed after it ever gets to attach its listener; polling current
      // state can't miss an event that already happened.
      await page.getByTestId("login-sandbox-starting").getByRole("button", { name: /cancel/i }).click();
      await expect.poll(() => tab.isClosed()).toBe(true);
    });

    // review-1 S3: the route-intercepted navigation — the extractor's own
    // host allowlist matches this URL (unlike the kind walk's fake), so this
    // is what proves the tab NAVIGATES, not merely that it opens.
    test("the verification URL navigates the tab already open", async ({ page, context }) => {
      const runId = "3f1b7c26-0000-4000-8000-00000000f014";
      await attachWithRealTerminal(page, context, runId);

      const pagePromise = context.waitForEvent("page");
      await page.getByRole("button", { name: /start login/i }).click();
      const tab = await pagePromise;

      await expect(tab).toHaveURL(new RegExp(`user_code=ABCD-EFGH`));
      await expect(page.getByTestId("auth-url-link")).toHaveAttribute("href", DEVICE_VERIFICATION_URL);
    });

    // review-1 S3: a repeated URL line (the sandbox's own tmux session can
    // reprint its banner on a reconnect) must navigate the SAME tab once,
    // never open a second one and never throw.
    test("a repeated URL line navigates once — no second tab, no page error", async ({ page, context }) => {
      const runId = "3f1b7c26-0000-4000-8000-00000000f015";
      await splicePerUserBedrock(page, "live");
      await page.route("**/api/v1/setup/harness-login", async (route) =>
        route.fulfill({ json: { run_id: runId, state: "PENDING" } }),
      );
      await page.route(`**/api/v1/runs/${runId}`, async (route) =>
        route.fulfill({ json: { id: runId, task: "harness login", state: "RUNNING", interactive: true } }),
      );
      await page.route(`**/api/v1/runs/${runId}/attach-ticket`, async (route) =>
        route.fulfill({ json: { ticket: "e2e-ticket" } }),
      );
      await page.routeWebSocket(/\/api\/v1\/runs\/[^/]+\/attach/, (ws) => {
        ws.send(JSON.stringify({ type: "attach-mode", read_only: false }));
        ws.send(Buffer.from(`${DEVICE_VERIFICATION_URL}\n`));
        // The SAME line again — the marker for "the sandbox reprinted itself",
        // not a second, different URL.
        ws.send(Buffer.from(`${DEVICE_VERIFICATION_URL}\n`));
      });
      await context.route(`${DEVICE_VERIFICATION_URL.split("?")[0]}**`, (route) =>
        route.fulfill({ contentType: "text/html", body: "<title>stub</title>" }),
      );
      const pageErrors: Error[] = [];
      page.on("pageerror", (e) => pageErrors.push(e));
      await gotoConsole(page);
      await navToRoute(page, "/settings");
      await page.locator("#lane-bedrock").click();
      await page.getByRole("button", { name: "Sign in with SSO" }).click();

      const newPages: Page[] = [];
      context.on("page", (p) => newPages.push(p));
      await page.getByRole("button", { name: /start login/i }).click();
      await expect(page.getByTestId("auth-url-link")).toBeVisible();
      // Give the second (duplicate) WS frame time to be processed.
      await page.waitForTimeout(200);

      expect(newPages).toHaveLength(1);
      expect(pageErrors).toHaveLength(0);
    });

    // review-1 B2, S3: once the tab has navigated cross-origin, close() is
    // BEST-EFFORT (the browser refuses it — the same tab-nabbing bound that
    // makes severing `opener` correct). Cancel must not crash the console
    // over a tab it can no longer close; the dialog still closes cleanly.
    test("Cancel after the tab has navigated is a harmless best-effort close", async ({ page, context }) => {
      const runId = "3f1b7c26-0000-4000-8000-00000000f016";
      await attachWithRealTerminal(page, context, runId);

      const pagePromise = context.waitForEvent("page");
      await page.getByRole("button", { name: /start login/i }).click();
      const tab = await pagePromise;
      await expect(tab).toHaveURL(new RegExp("user_code=ABCD-EFGH"));

      await page.getByRole("button", { name: /cancel/i }).click();
      await expect(page.getByRole("dialog")).toHaveCount(0);
    });

    // A person who closes the auto-opened tab by hand must never crash the
    // pane on the NEXT thing that tries to touch it (a later navigate/close
    // call) — auth-tab-handle.ts's own try/catch is pinned in vitest; this is
    // the live-browser half: the console keeps working afterwards.
    test("a user-closed tab does not break the pane — Cancel still works", async ({ page, context }) => {
      const runId = "3f1b7c26-0000-4000-8000-00000000f013";
      await openStartingPane(page, runId);
      const pagePromise = context.waitForEvent("page");
      await page.getByRole("button", { name: /start login/i }).click();
      const tab = await pagePromise;
      await tab.close();

      await expect(page.getByTestId("login-sandbox-starting")).toBeVisible();
      await page.getByTestId("login-sandbox-starting").getByRole("button", { name: /cancel/i }).click();
      await expect(page.getByRole("dialog")).toHaveCount(0);
      await expect(page.getByRole("button", { name: "Sign in with SSO" })).toBeVisible();
    });
  });

  // Finding 6 (0.7.4 field report): the wait's OTHER end. A first pull of the
  // aws-sso image measured 131s on the reporting estate — healthy reads
  // throughout — and the pane narrated it with the same one-line "Starting…"
  // until its 15-tick budget expired and accused the daemon of being
  // unreadable. Past a minute the pane now says which wait it is in, and says
  // nothing about a failure it has no evidence for.
  //
  // page.clock, not a real 65-second wait: the pane grades the wait on
  // Date.now() and usePoll's setInterval, both of which the clock API fakes, so
  // this stays a sub-second case. `-runner none` keeps the run PENDING forever,
  // which is exactly the shape being narrated.
  test("past a minute of healthy reads the sign-in pane says it is slow, never that it failed", async ({ page }) => {
    const loginRunId = "3f1b7c26-0000-4000-8000-00000000f003";
    await page.clock.install();
    await splicePerUserBedrock(page, "live");
    await page.route("**/api/v1/setup/harness-login", async (route) =>
      route.fulfill({ json: { run_id: loginRunId, state: "PENDING" } }),
    );
    // Every poll answers, and answers PENDING: nothing here is failing.
    await page.route(`**/api/v1/runs/${loginRunId}`, async (route) =>
      route.fulfill({ json: { id: loginRunId, task: "harness login", state: "PENDING", interactive: true } }),
    );

    await gotoConsole(page);
    await navToRoute(page, "/settings");
    await page.locator("#lane-bedrock").click();
    await page.getByRole("button", { name: "Sign in with SSO" }).click();
    await page.getByRole("button", { name: /start login/i }).click();

    const starting = page.getByTestId("login-sandbox-starting");
    await expect(starting).toContainText(LOGIN_SANDBOX_STARTING);

    await page.clock.fastForward("01:10");
    await expect(starting).toContainText(LOGIN_SANDBOX_SLOW_START);
    // The old budget fired at ~30s of ticks; this one must not fire at all
    // while the run is readable — not now, and not five minutes from now.
    await expect(page.getByRole("alert")).toHaveCount(0);
    await page.clock.fastForward("05:00");
    await expect(page.getByRole("alert")).toHaveCount(0);
    await expect(starting).toContainText(LOGIN_SANDBOX_SLOW_START);
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

// #337: a MEMBER on a per_user Bedrock BEARER row can edit their own bearer
// field — the console's missing half of #153/#327's server-side write door
// (member writes to bedrock-api-key are already admitted there).
//
// PR #352 review, finding 1: the FIRST version of this block called
// mockMemberRole (fixtures.ts, splices GET /me's role/operator fields, plus
// its own redacting **/api/v1/setup/status* route) and then spliceBedrockRow
// on the SAME status pattern. spliceBedrockRow runs LAST-registered-wins
// (Playwright routes are LIFO) and its handler calls route.fetch() itself —
// which hits the network directly rather than falling through to
// mockMemberRole's handler — so the redaction never ran; the test read the
// RAW ADMIN body the whole time. That hid the real bug: `st.Bedrock =
// SetupBedrock{Ready: st.Bedrock.Ready}` (internal/api/setup.go) zeroed
// BearerPresent for every non-operator unconditionally, so a real member's
// Save never showed Replace/Disconnect — the field looked stored under the
// unredacted splice and came back empty under the real one.
//
// mockMemberBedrockRowRedacted below is this file's own composed splice
// instead: ONE **/api/v1/setup/status* handler that mirrors
// redactSetupStatusForMember's structural drops (the same shape
// mockMemberSetupStatus, fixtures.ts, mirrors for every OTHER member spec in
// this repo) AND injects the harnesses row, so nothing here can bypass the
// redaction the way stacking two routes on the same pattern did.
async function mockMemberBedrockRowRedacted(
  page: Page,
  credentialSource: "per_user" | "shared",
  mechanism: "bedrock_sso" | "bedrock_bearer",
  initialBearerPresent: boolean,
): Promise<void> {
  await page.route("**/api/v1/me", async (route) => {
    const response = await route.fetch();
    const json = await response.json();
    json.role = "user";
    json.operator = false;
    json.security_operator = false;
    await route.fulfill({ response, json });
  });

  // Mutable, not cached-once: a member's real Save/Disconnect below hits the
  // real backend (this harness's bearer token is admin server-side, so the
  // write itself always succeeds — the point being proven is only that the
  // CONSOLE re-reads a body shaped the way redaction really answers it, not a
  // genuine per-member namespaced write, same ceiling as every mockMemberRole
  // spec in this file). Flipped by the secrets-endpoint splice below so the
  // NEXT poll reflects it, the way a real member's own redacted read would.
  let bearerPresent = initialBearerPresent;
  await page.route("**/api/v1/setup/status*", async (route) => {
    const body = (await (await route.fetch()).json()) as Record<string, unknown>;
    // Mirrors redactSetupStatusForMember (internal/api/setup.go) — the same
    // drop list mockMemberSetupStatus (fixtures.ts) applies for every other
    // member spec, plus the #337 BearerPresent carve-out that function now
    // applies under the caller's own per_user bearer row.
    body.checks = [];
    body.checks_redacted = true;
    body.providers = [];
    body.secrets = { present: [] };
    const runner = (body.runner ?? {}) as { confinement_classes?: string[] };
    body.runner = { confinement_classes: runner.confinement_classes ?? [] };
    const ready = !!(body.bedrock as { ready?: boolean } | undefined)?.ready;
    body.bedrock = { ready, creds_present: false, bearer_present: bearerPresent };
    body.scm = {};
    body.host_proxy = {};
    body.deployment = {};
    body.harnesses = [
      {
        id: "claude-code",
        display: "Claude Code",
        has_gateway: true,
        has_login: true,
        enabled: true,
        mechanism,
        credential_source: credentialSource,
      },
    ];
    await route.fulfill({ json: body });
  });

  await page.route("**/api/v1/secrets/bedrock-api-key", async (route) => {
    const response = await route.fetch();
    if (response.ok()) {
      if (route.request().method() === "PUT") bearerPresent = true;
      if (route.request().method() === "DELETE") bearerPresent = false;
    }
    await route.fulfill({ response });
  });
}

test.describe("providers — #337: a member's own Bedrock bearer field under a per_user bearer row", () => {
  test("editable on a per_user bearer row", async ({ page }) => {
    await mockMemberBedrockRowRedacted(page, "per_user", "bedrock_bearer", false);
    await gotoConsole(page);
    await navToRoute(page, "/settings");
    await page.locator("#lane-bedrock").click();
    await expect(page.getByLabel("Bedrock bearer key")).toBeEditable();
  });

  test("still disabled on a shared row", async ({ page }) => {
    await mockMemberBedrockRowRedacted(page, "shared", "bedrock_bearer", false);
    await gotoConsole(page);
    await navToRoute(page, "/settings");
    await page.locator("#lane-bedrock").click();
    await expect(page.getByLabel("Bedrock bearer key")).toBeDisabled();
  });

  // PR #352 review, finding 1's own live-browser reproduction: a member's
  // Save must lead to Replace/Disconnect, reading the body the way the
  // server's redaction really answers it — not the raw admin body the first
  // version of this block accidentally read (see the block comment above).
  test("Save leads to Replace and Disconnect for a member, reading the redacted body", async ({ page }) => {
    await mockMemberBedrockRowRedacted(page, "per_user", "bedrock_bearer", false);
    await gotoConsole(page);
    await navToRoute(page, "/settings");
    await page.locator("#lane-bedrock").click();

    const field = page.getByLabel("Bedrock bearer key");
    await expect(field).toBeEditable();
    await field.fill("e2e-member-bearer-token");
    await page.getByRole("button", { name: "Save", exact: true }).click();

    await expect(page.getByText(/Saved bedrock-api-key/)).toBeVisible();
    await expect(page.getByRole("button", { name: "Replace" })).toBeVisible();
    await expect(page.getByRole("button", { name: "Disconnect" })).toBeVisible();
  });
});

// UT-7b — the "Available to" control embedded in the git provider row: real
// writes against /permissions/availability and /permissions/grants, proving
// the round trip an admin actually performs (not just this file's own
// component-level coverage in availability-control.test.tsx). The github row
// already exists by this point in the file (the serial walk above never
// removes it), so this reuses it rather than adding a third row.
test.describe("providers — the git provider row's Available to control (UT-7b)", () => {
  test.describe.configure({ mode: "serial" });

  test("Everyone by default; turning Only on with nobody listed refuses, verbatim", async ({ page }) => {
    await gotoProviders(page);
    const row = page.getByTestId("provider-row-github");
    await expect(row).toBeVisible();

    await expect(row.getByRole("button", { name: AVAILABILITY.EVERYONE })).toHaveAttribute("aria-pressed", "true");
    await row.getByRole("button", { name: AVAILABILITY.ONLY }).click();

    // The server's own availabilityOnlyEmptyMsg (permissions_availability.go),
    // rendered verbatim — never a console reword.
    await expect(
      row.getByText("Add at least one person, group or user type before choosing Only, or nobody could use this."),
    ).toBeVisible();
    // The failed PUT never flipped the segment — it still reads Everyone.
    await expect(row.getByRole("button", { name: AVAILABILITY.EVERYONE })).toHaveAttribute("aria-pressed", "true");
  });

  test("adding a user type lands the chip, turns Only on, and both survive a reload", async ({ page }) => {
    await gotoProviders(page);
    const row = page.getByTestId("provider-row-github");
    await expect(row).toBeVisible();

    // "standard" — the one user type every deployment seeds and can never
    // delete (UT-1) — is the only type id guaranteed to exist on a fresh e2e
    // backend: userTypeSubjectExists (permissions.go) refuses a grant naming
    // an id nobody created.
    await row.getByPlaceholder(AVAILABILITY.ADD_PLACEHOLDER).fill("standard");
    await row.getByRole("button", { name: AVAILABILITY.ADD_CTA }).click();
    await expect(row.getByText(/standard/)).toBeVisible();

    await row.getByRole("button", { name: AVAILABILITY.ONLY }).click();
    await expect(row.getByRole("button", { name: AVAILABILITY.ONLY })).toHaveAttribute("aria-pressed", "true");

    await page.reload();
    const reloaded = page.getByTestId("provider-row-github");
    await expect(reloaded).toBeVisible();
    await expect(reloaded.getByRole("button", { name: AVAILABILITY.ONLY })).toHaveAttribute("aria-pressed", "true");
    await expect(reloaded.getByText(/standard/)).toBeVisible();
    // The mock's two provider lines, and the last audience locked while Only is on.
    await expect(reloaded.getByText(AVAILABILITY.PROVIDER_ONLY_HINT)).toBeVisible();
    await expect(reloaded.getByText(AVAILABILITY.PROVIDER_NOTE)).toBeVisible();
    await expect(reloaded.getByRole("button", { name: /Remove.*standard/i })).toBeDisabled();
    await expect(reloaded.getByText(AVAILABILITY.LAST_AUDIENCE_LOCKED)).toBeVisible();

    // The wire itself — restricted, and the allow row naming this exact kind/value.
    const view = await (
      await page.request.get("/api/v1/permissions/availability/workspace_provider/github", { headers: auth })
    ).json();
    expect(view.restricted).toBe(true);
    expect(view.allowed_by).toEqual([
      expect.objectContaining({ subject_type: "user_type", subject: "standard", capability: "workspace_provider", value: "github" }),
    ]);

    // Clean up: back to Everyone, then remove the audience — this row's
    // availability must not leak into a later run of this same suite.
    await reloaded.getByRole("button", { name: AVAILABILITY.EVERYONE }).click();
    await expect(reloaded.getByRole("button", { name: AVAILABILITY.EVERYONE })).toHaveAttribute("aria-pressed", "true");
    await reloaded.getByRole("button", { name: /Remove.*standard/i }).click();
    await expect(reloaded.getByText(/standard/)).toHaveCount(0);
  });
});
