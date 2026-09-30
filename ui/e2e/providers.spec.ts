/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import {
  test,
  expect,
  ADMIN_TOKEN,
  expandCard,
  gotoConsole,
  mockMemberRole,
  mockSecurityAdminRole,
  navToRoute,
  sidebarLink,
} from "./fixtures";
import { ADO_CAP_COPY, ADO_ENTRA_EDITOR, PROVIDERS, PROVIDERS_EXTRA } from "../src/app/lib/workspace-providers-copy";
import { AVAILABILITY } from "../src/app/lib/availability-copy";
import { OPERATOR_ONLY_REASON, UNSAVED_GUARD } from "../src/app/components/wardyn/copy";
import { VIEW_REFUSAL } from "../src/app/components/wardyn/copy/console-view";
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
  await navToRoute(page, "/admin/settings");
  const card = page.getByTestId("providers-card");
  await expect(card).toBeVisible();
  await expandCard(page, "Workspace providers");
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
// PRE-save document in 4 of 20 repeats on an otherwise idle box (the snapshot
// GET was logged ~26ms BEFORE the PUT's own 200). The reload in the
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
    await navToRoute(page, "/admin/settings");
    const card = page.getByTestId("providers-card");
    await expect(card).toBeVisible();
    await expandCard(page, "Workspace providers");
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

    // #460 (Q460-3) — reversed from #217's "changed fields only" rule: the
    // banner shows the WHOLE document (the edited row included), and Copy my
    // changes puts exactly that on the clipboard, never just the diff. The
    // <pre> is the banner's own preview — the same text also appears in the
    // (still-mounted) textarea and the row's collapsed summary, so this is
    // scoped to it rather than page.getByText, which would hit all three.
    const documentPreview = page.locator("pre");
    await expect(documentPreview).toContainText("https://github.com/acme");
    await expect(documentPreview).toContainText("https://git.corp.example/team");
    await page.getByRole("button", { name: PROVIDERS_EXTRA.CONFLICT_COPY }).click();
    await expect(page.getByText(PROVIDERS_EXTRA.CONFLICT_COPIED_TOAST)).toBeVisible();
    const clipboardText = await page.evaluate(() => navigator.clipboard.readText());
    const clipboardDraft = JSON.parse(clipboardText);
    const githubRow = (clipboardDraft.git as { base_urls?: string[] }[]).find((g) =>
      (g.base_urls ?? []).includes("https://github.com/acme"),
    );
    expect(githubRow?.base_urls).toEqual(["https://github.com/acme", "https://git.corp.example/team"]);

    // Discard mine and reload is still there, now beside Copy, not the only
    // exit.
    await expect(page.getByRole("button", { name: PROVIDERS_EXTRA.DISCARD_AND_RELOAD })).toBeVisible();

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
    // #460 added the same "Unsaved changes" chip in three more places
    // (PageHeader, Git tab, Storage tab), so this targets the beside-Save
    // marker by its own testid rather than the now-ambiguous text.
    await expect(page.getByTestId("unsaved-marker")).toHaveText(PROVIDERS_EXTRA.UNSAVED_MARKER);

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
    await navToRoute(page, "/admin/providers");
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
    await navToRoute(page, "/admin/providers");
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
    // The funnel step lives at /admin/setup since M-6/D1 — plain /setup is
    // the User Getting Started now, even for this harness's admin session.
    await navToRoute(page, "/admin/setup");
    const stepBtn = page.getByRole("button", { name: new RegExp(`^${PROVIDERS.STEP_LABEL}`) });
    await expect(stepBtn).toBeVisible();
    await expect(stepBtn).toContainText(PROVIDERS.STEP_BADGE_READY(enabledCount));

    await navToRoute(page, "/admin/settings");
    const card = page.getByTestId("providers-card");
    await expect(card).toBeVisible();
    await expandCard(page, "Workspace providers");
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
    await navToRoute(page, "/admin/settings");
    await expect(page.getByTestId("providers-card")).toHaveCount(0);

    // Reaching /providers directly: GET is operatorOnly, so a real security
    // admin's read genuinely answers 403 — routed here as the real server's
    // shape (the role splice leaves the bearer admin, exactly as
    // drives.spec.ts's equivalent test documents).
    await page.route("**/api/v1/workspace-providers", async (route) => {
      if (route.request().method() !== "GET") return route.fallback();
      await route.fulfill({ status: 403, contentType: "application/json", body: JSON.stringify({ error: "forbidden" }) });
    });
    await navToRoute(page, "/admin/providers");
    await expect(page.getByRole("heading", { name: PROVIDERS.TITLE, level: 1 })).toBeVisible();
    await expect(page.getByText(OPERATOR_ONLY_REASON)).toBeVisible();
    // No form of any kind — no tabs, no Save, no rows.
    await expect(page.getByRole("button", { name: PROVIDERS.SAVE_CTA })).toHaveCount(0);
    await expect(page.getByTestId("provider-row-github")).toHaveCount(0);
    await expect(page.getByRole("button", { name: PROVIDERS.GIT_TITLE })).toHaveCount(0);
  });

  // M-1b: /providers is deleted — only /admin/providers exists now, so a
  // member reaching it directly hits the admin-view REFUSAL page
  // (console-view.tsx) before anything is fetched, not the screen's own
  // inline OPERATOR_ONLY_REASON (that still covers the security-admin case
  // above, whose SSO tier passes the view gate but not the server's operator
  // check).
  test("a member sees no nav, no card, and the admin-view refusal page if they reach the URL directly", async ({ page }) => {
    await mockMemberRole(page);
    await gotoConsole(page);

    // A member has no Settings screen at all (MEMBER_NAV_PATHS never lists
    // it, app-shell.tsx), so there is no card to check there — the negative
    // worth pinning is the route itself.
    let fetched = false;
    await page.route("**/api/v1/workspace-providers", async (route) => {
      fetched = true;
      await route.fallback();
    });
    await navToRoute(page, "/admin/providers");
    await expect(page.getByRole("heading", { name: VIEW_REFUSAL.TITLE })).toBeVisible();
    await expect(page.getByText(VIEW_REFUSAL.BODY)).toBeVisible();
    await expect(page.getByRole("heading", { name: PROVIDERS.TITLE, level: 1 })).toHaveCount(0);
    expect(fetched).toBe(false);
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

    await expect(row.getByRole("radio", { name: AVAILABILITY.EVERYONE })).toBeChecked();
    await row.getByRole("radio", { name: AVAILABILITY.ONLY }).click();

    // The server's own availabilityOnlyEmptyMsg (permissions_availability.go),
    // rendered verbatim — never a console reword.
    await expect(
      row.getByText("Add at least one person, group or user type before choosing Only, or nobody could use this."),
    ).toBeVisible();
    // The failed PUT never flipped the segment — it still reads Everyone.
    await expect(row.getByRole("radio", { name: AVAILABILITY.EVERYONE })).toBeChecked();
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
    await expect(row.getByText(/standard/i)).toBeVisible();

    await row.getByRole("radio", { name: AVAILABILITY.ONLY }).click();
    await expect(row.getByRole("radio", { name: AVAILABILITY.ONLY })).toBeChecked();

    await page.reload();
    const reloaded = page.getByTestId("provider-row-github");
    await expect(reloaded).toBeVisible();
    await expect(reloaded.getByRole("radio", { name: AVAILABILITY.ONLY })).toBeChecked();
    await expect(reloaded.getByText(/standard/i)).toBeVisible();
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
    await reloaded.getByRole("radio", { name: AVAILABILITY.EVERYONE }).click();
    await expect(reloaded.getByRole("radio", { name: AVAILABILITY.EVERYONE })).toBeChecked();
    await reloaded.getByRole("button", { name: /Remove.*standard/i }).click();
    await expect(reloaded.getByText(/standard/i)).toHaveCount(0);
  });
});

// G1: the Azure DevOps row's Entra section, on a real backend. The row is
// seeded over the API (the lane itself is not switched on from this screen);
// the ceiling and the default are then set in the browser, saved, reloaded,
// and read back from both the screen and the wire.
test.describe("providers — the Azure DevOps Entra section (real writes, real reload)", () => {
  test("setting the ceiling and the default persists across a reload", async ({ page }) => {
    await resetProviders(page);
    const seed = (caps: string[]) => ({
      git: [
        {
          id: "azure_devops",
          kind: "azure_devops",
          base_urls: ["https://dev.azure.com/wardyn-e2e"],
          lanes: ["entra"],
          credential_source: "per_user",
          entra: {
            tenant_id: "8f14e45f-ceea-4d2c-a3f9-1a2b3c4d5e6f",
            client_id: "3b241101-e2bb-4255-8caf-4136c566a962",
            capability_ceiling: caps,
            default_profile: caps,
          },
        },
      ],
    });
    const etag = (await page.request.get("/api/v1/workspace-providers", { headers: auth })).headers()["etag"];
    const ifMatch = etag ? { ...auth, "If-Match": etag } : auth;
    // The pre-split "read" is a clean break: refused, never aliased.
    const old = await page.request.put("/api/v1/workspace-providers", { headers: ifMatch, data: seed(["read"]) });
    expect(old.status()).toBe(400);
    const put = await page.request.put("/api/v1/workspace-providers", {
      headers: ifMatch,
      data: seed(["code_read", "project_read"]),
    });
    expect(put.status()).toBe(200);

    await gotoProviders(page);
    const row = page.getByTestId("provider-row-azure_devops");
    const ceiling = row.getByRole("group", { name: ADO_ENTRA_EDITOR.CEILING_TITLE });
    const defaults = row.getByRole("group", { name: ADO_ENTRA_EDITOR.DEFAULT_TITLE });
    const push = ADO_CAP_COPY.code_write.name;
    const policy = ADO_CAP_COPY.policy_admin.name;

    // Off the ceiling, so its default is locked until the ceiling admits it.
    await expect(defaults.getByRole("checkbox", { name: push })).toBeDisabled();
    await ceiling.getByRole("checkbox", { name: push }).click();
    await ceiling.getByRole("checkbox", { name: policy }).click();
    await defaults.getByRole("checkbox", { name: push }).click();
    await saveProviders(page);

    await page.reload();
    const reloaded = page.getByTestId("provider-row-azure_devops");
    const ceiling2 = reloaded.getByRole("group", { name: ADO_ENTRA_EDITOR.CEILING_TITLE });
    const defaults2 = reloaded.getByRole("group", { name: ADO_ENTRA_EDITOR.DEFAULT_TITLE });
    await expect(ceiling2.getByRole("checkbox", { name: push })).toBeChecked();
    await expect(ceiling2.getByRole("checkbox", { name: policy })).toBeChecked();
    await expect(defaults2.getByRole("checkbox", { name: push })).toBeChecked();
    await expect(defaults2.getByRole("checkbox", { name: policy })).not.toBeChecked();

    const snap = await (await page.request.get("/api/v1/workspace-providers", { headers: auth })).json();
    expect(snap.git[0].entra).toEqual(
      expect.objectContaining({
        capability_ceiling: ["code_read", "code_write", "policy_admin", "project_read"],
        default_profile: ["code_read", "code_write", "project_read"],
      }),
    );

    await resetProviders(page);
  });
});
