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
import { PROVIDERS } from "../src/app/lib/workspace-providers-copy";
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
    await page.getByRole("button", { name: PROVIDERS.SAVE_CTA }).click();
    await expect(page.getByText(PROVIDERS.SAVE_ERROR)).toHaveCount(0);

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
    await page.getByRole("button", { name: PROVIDERS.SAVE_CTA }).click();
    await expect(page.getByText(PROVIDERS.SAVE_ERROR)).toHaveCount(0);
    await expect(page.getByText(PROVIDERS.SAVE_REFUSED_TITLE)).toHaveCount(0);

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
