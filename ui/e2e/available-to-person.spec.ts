/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #922 (UT-7c) — the person side of "Available to", against a real backend:
// a git provider (Azure DevOps) restricted to one user type, and New Run's
// own Launch button for a workspace-kind restriction.
//
// Two pins on the git-provider family, one family, real writes and real
// refusals (never page.route):
//   1. "A user-type session sees no ADO repos" — a restricted type's every
//      attempt to bring in a repo on the restricted org is refused; the type
//      actually listed succeeds. There is no repo-BROWSING surface in the
//      console today (Add workspace is a plain URL field, never a picker), so
//      this is the real, provable shape of "sees no repos": no ADO repo ever
//      lands for them, on any of several distinct repos under that org.
//   2. "A pasted ADO URL in Add workspace renders the server's frozen refusal
//      verbatim" — the same restriction, reached through the console's own
//      Add workspace dialog, asserting the toast carries the server's exact
//      403 body (getErrorMessage passes it through untouched — see
//      add-workspace-dialog.tsx's own catch block).
//
// Why Azure DevOps and not GitHub: the issue and its mock both use Azure
// DevOps as the worked example. A SHARED (non-Entra) row is enough for both
// pins — capWorkspaceProvider and repo admission need no per-user sign-in at
// all; only the CONNECTION-STATE surfaces (GET /me/scm-access) need a real
// Entra tenant, which scripts/e2e-backend.sh does not run (see
// ado-launch-door.spec.ts's own comment) — neither pin here touches that
// surface.
import { createHash, randomBytes } from "node:crypto";
import { test, expect, ADMIN_TOKEN, TOKEN_KEY, gotoConsole, navTo, navToRoute, sql } from "./fixtures";
import { DENIED } from "../src/app/lib/permissions-copy";

const auth = { Authorization: `Bearer ${ADMIN_TOKEN}` };

// An operator-chosen slug (types.GitProvider.ID's own doc comment: "an
// operator-chosen stable slug") — distinct from any row another spec file
// might seed, so this file never depends on run order.
const PROVIDER_ID = "e2e-922-ado";
const ORG_BASE_URL = "https://dev.azure.com/e2e922";
const LISTED_TYPE = "e2e-922-portfolio-manager";

function adoRepoURL(project: string, repo: string): string {
  return `${ORG_BASE_URL}/${project}/_git/${repo}`;
}

// seedUserToken mirrors available-to.spec.ts's own helper byte-for-byte
// (kept local rather than imported: the two spec files seed unrelated rows
// and must never share a mutable module). A genuine user-tier row, keyed by
// the token's sha256 exactly as apiTokenAuth stores one.
function seedUserTokenRaw(userType: string): string {
  const raw = `wdn_${randomBytes(32).toString("hex")}`;
  const hash = createHash("sha256").update(raw).digest("hex");
  const who = `${userType}-${hash.slice(0, 8)}@e2e.test`;
  sql(
    `INSERT INTO api_tokens (id, principal, email, role, user_type, groups, groups_truncated, name, token_sha256, created_at)
     VALUES (gen_random_uuid(), '${who}', '${who}', 'user', '${userType}', '[]'::jsonb, false, 'e2e', '${hash}', now())`,
  );
  return raw;
}

test.describe("Available to — a person's git provider (#922)", () => {
  test.describe.configure({ mode: "serial" });
  let restrictedToken = "";
  let listedToken = "";

  test.beforeAll(async ({ request }) => {
    // A shared (non-Entra) Azure DevOps row — one PUT, additive to whatever
    // this backend already carries (GET-then-merge, the resetProviders
    // idiom providers.spec.ts uses, so this never clobbers another file's row).
    const before = await request.get("/api/v1/workspace-providers", { headers: auth });
    const cur = await before.json();
    const git = (cur.git ?? []).filter((g: { id: string }) => g.id !== PROVIDER_ID);
    git.push({ id: PROVIDER_ID, kind: "azure_devops", base_urls: [ORG_BASE_URL] });
    const put = await request.put("/api/v1/workspace-providers", {
      headers: { ...auth, "If-Match": before.headers()["etag"] ?? "" },
      data: { git, storage: cur.storage },
    });
    expect(put.status(), await put.text()).toBe(200);

    // The listed type, and the restriction naming only it.
    const madeType = await request.post("/api/v1/user-types", {
      headers: auth,
      data: { id: LISTED_TYPE, name: "E2E portfolio manager" },
    });
    expect([201, 409], await madeType.text()).toContain(madeType.status());
    const granted = await request.post("/api/v1/permissions/grants", {
      headers: auth,
      data: { subject_type: "user_type", subject: LISTED_TYPE, capability: "workspace_provider", value: PROVIDER_ID, effect: "allow" },
    });
    expect(granted.status(), await granted.text()).toBe(201);
    const restricted = await request.put(`/api/v1/permissions/availability/workspace_provider/${PROVIDER_ID}`, {
      headers: auth,
      data: { restricted: true },
    });
    expect(restricted.status(), await restricted.text()).toBe(200);

    restrictedToken = seedUserTokenRaw("standard");
    listedToken = seedUserTokenRaw(LISTED_TYPE);
  });

  test.afterAll(async ({ request }) => {
    // Leave the deployment as this file found it: Everyone again, then the
    // row itself gone (never the reverse — an empty "Only" list is refused).
    await request.put(`/api/v1/permissions/availability/workspace_provider/${PROVIDER_ID}`, {
      headers: auth,
      data: { restricted: false },
    });
    const before = await request.get("/api/v1/workspace-providers", { headers: auth });
    const cur = await before.json();
    const git = (cur.git ?? []).filter((g: { id: string }) => g.id !== PROVIDER_ID);
    await request.put("/api/v1/workspace-providers", {
      headers: { ...auth, "If-Match": before.headers()["etag"] ?? "" },
      data: { git, storage: cur.storage },
    });
  });

  // Pin 1 — "a user-type session sees no ADO repos": every repo this
  // restricted session brings in from the restricted org is refused, on
  // three distinct repos (never a fluke on one path), while the type
  // actually named on the availability list succeeds on the same repos.
  test("a restricted user type brings in no ADO repos; the listed type brings in every one", async ({
    request,
  }) => {
    const attempt = (token: string, repo: string) =>
      request.post("/api/v1/workspaces", {
        headers: { Authorization: `Bearer ${token}` },
        data: { name: `e2e-922-${repo}`, sources: [{ type: "repo", source: adoRepoURL("risk", repo) }] },
      });

    for (const repo of ["models", "pricing", "reports"]) {
      const refused = await attempt(restrictedToken, repo);
      const body = await refused.text();
      expect(refused.status(), body).toBe(403);
      // The restriction is what refused it, not some other 403 (a malformed
      // URL, a legacy-host refusal): the sentence names the provider's KIND.
      expect(JSON.parse(body).error).toContain("azure_devops");
    }

    for (const repo of ["models", "pricing", "reports"]) {
      const ok = await attempt(listedToken, repo);
      const body = await ok.text();
      expect(ok.status(), body).toBe(201);
      await request.delete(`/api/v1/workspaces/${JSON.parse(body).id}`, { headers: auth });
    }
  });

  // Pin 2 — "a pasted ADO URL in Add workspace renders the server's frozen
  // refusal verbatim": the SAME 403 body, this time read through the
  // console's own Add workspace dialog as a live restricted session, proving
  // getErrorMessage never rewords it (add-workspace-dialog.tsx's catch).
  test("Add workspace, pasted: the restricted session's toast is the server's 403 body, verbatim", async ({
    page,
  }) => {
    // The exact server sentence, captured independently of the UI under
    // test — never hand-retyped, so a wording change in
    // internal/api/workspace_providers.go's capProvider403 breaks this
    // spec instead of letting the console silently drift from it.
    const direct = await page.request.post("/api/v1/workspaces", {
      headers: { Authorization: `Bearer ${restrictedToken}` },
      data: { name: "e2e-922-direct-probe", sources: [{ type: "repo", source: adoRepoURL("risk", "models") }] },
    });
    expect(direct.status()).toBe(403);
    const serverSentence = JSON.parse(await direct.text()).error as string;

    await page.addInitScript(([key, tok]) => localStorage.setItem(key, tok), [TOKEN_KEY, restrictedToken]);
    await gotoConsole(page);
    await navTo(page, "Workspaces");
    await page.getByRole("button", { name: "Add workspace" }).click();
    const dialog = page.getByRole("dialog");
    await dialog.getByLabel("Repository URL").fill(adoRepoURL("risk", "models"));
    await dialog.getByRole("button", { name: "Add workspace" }).click();

    await expect(page.getByText(serverSentence)).toBeVisible();
    // The dialog stays open on the refusal — nothing was created.
    await expect(dialog).toBeVisible();
  });
});

// New Run's own Launch button, the half this file's earlier round held back
// (new-run-screen.tsx's `problem` derivation was DO NOT TOUCH while #1228/
// #1229 — the shared TierPicker/barrier lanes — were still open; both landed
// on main via batch 08-13, 37ba7e78a). This exercises the "workspace itself
// carries no allow" reason (capabilityAllowed's existing "workspace" kind,
// now ALSO wired to Launch, not just the picker's own advisory line) — a
// REAL backend capability restriction, not the model-provider half
// (workspace-card.test.tsx / new-run-screen-form.test.tsx already pin that
// one as a component test, same reasoning as available-to.spec.ts's own
// "component test, not e2e" note for wire-shaped states).
test.describe("Available to — New Run's Launch button (#922)", () => {
  test.describe.configure({ mode: "serial" });
  const WORKSPACE_KIND_TYPE = "e2e-922-nr-listed";
  let workspaceId = "";
  let grantId = "";
  let originalEnforcement: Record<string, boolean> = {};

  test.beforeAll(async ({ request }) => {
    const created = await request.post("/api/v1/workspaces", {
      headers: auth,
      data: { name: "e2e-922-nr-workspace", sources: [{ type: "ephemeral", target: "/home/agent/work" }] },
    });
    expect(created.status(), await created.text()).toBe(201);
    workspaceId = (await created.json()).id;

    const madeType = await request.post("/api/v1/user-types", {
      headers: auth,
      data: { id: WORKSPACE_KIND_TYPE, name: "E2E New Run listed" },
    });
    expect([201, 409], await madeType.text()).toContain(madeType.status());
    const granted = await request.post("/api/v1/permissions/grants", {
      headers: auth,
      data: { subject_type: "user_type", subject: WORKSPACE_KIND_TYPE, capability: "workspace", value: workspaceId, effect: "allow" },
    });
    expect(granted.status(), await granted.text()).toBe(201);
    grantId = (await granted.json()).id;

    // The "workspace" KIND's own enforcement switch (permissions.tsx's older,
    // kind-wide control — distinct from UT-10's per-VALUE restricted bit):
    // GET /me/capabilities carries the caller's own allow rows but no
    // per-value restricted signal (#1018-adjacent gap, documented in this
    // PR's body), so the client's capabilityAllowed only reads "denied" once
    // the KIND itself is enforced. A real admin enabling it, merged onto
    // whatever this deployment already enforces — never a blind replacement
    // (PutCapabilityEnforcement is a full-document PUT).
    const before = await request.get("/api/v1/permissions", { headers: auth });
    originalEnforcement = (await before.json()).enforcement ?? {};
    const put = await request.put("/api/v1/permissions/enforcement", {
      headers: { ...auth, "If-Match": before.headers()["etag"] ?? "" },
      data: { ...originalEnforcement, workspace: true },
    });
    expect(put.status(), await put.text()).toBe(200);
  });

  test.afterAll(async ({ request }) => {
    const before = await request.get("/api/v1/permissions", { headers: auth });
    await request.put("/api/v1/permissions/enforcement", {
      headers: { ...auth, "If-Match": before.headers()["etag"] ?? "" },
      data: originalEnforcement,
    });
    if (grantId) await request.delete(`/api/v1/permissions/grants/${grantId}`, { headers: auth });
    if (workspaceId) await request.delete(`/api/v1/workspaces/${workspaceId}`, { headers: auth });
  });

  test("a caller with no allow for the workspace: Launch is disabled with the canon sentence", async ({ page }) => {
    await page.addInitScript(([key, tok]) => localStorage.setItem(key, tok), [TOKEN_KEY, seedUserTokenRaw("standard")]);
    await gotoConsole(page);
    await navToRoute(page, "/runs/new");
    await page.getByRole("combobox", { name: "Workspace" }).click();
    await page.getByRole("option", { name: "e2e-922-nr-workspace" }).click();

    const launch = page.getByRole("button", { name: "Launch run" });
    await expect(launch).toBeDisabled();
    await expect(page.getByText(DENIED.WORKSPACE_NOT_AVAILABLE)).toBeVisible();
  });

  test("the listed type: Launch stays enabled, the sentence never renders", async ({ page }) => {
    await page.addInitScript(([key, tok]) => localStorage.setItem(key, tok), [TOKEN_KEY, seedUserTokenRaw(WORKSPACE_KIND_TYPE)]);
    await gotoConsole(page);
    await navToRoute(page, "/runs/new");
    await page.getByRole("combobox", { name: "Workspace" }).click();
    await page.getByRole("option", { name: "e2e-922-nr-workspace" }).click();

    const launch = page.getByRole("button", { name: "Launch run" });
    await expect(launch).toBeEnabled();
    await expect(page.getByText(DENIED.WORKSPACE_NOT_AVAILABLE)).toHaveCount(0);
  });
});

// #1267 (closes #1250's design note too): the PER-VALUE "Available to: Only
// these" restriction — set on the WORKSPACE ITSELF via
// PUT /permissions/availability/workspace/{id}, the kind-wide switch left
// OFF throughout. This is exactly the gap #922's own review found and #1267's
// available_to_you closes: the block above already pins the kind-wide
// switch; this one pins the per-value bit the switch does nothing for.
test.describe("Available to — a workspace's own per-value restriction (#1267)", () => {
  test.describe.configure({ mode: "serial" });
  const LISTED_TYPE = "e2e-1267-listed";
  let workspaceId = "";
  let grantId = "";

  test.beforeAll(async ({ request }) => {
    const created = await request.post("/api/v1/workspaces", {
      headers: auth,
      data: { name: "e2e-1267-workspace", sources: [{ type: "ephemeral", target: "/home/agent/work" }] },
    });
    expect(created.status(), await created.text()).toBe(201);
    workspaceId = (await created.json()).id;

    const madeType = await request.post("/api/v1/user-types", {
      headers: auth,
      data: { id: LISTED_TYPE, name: "E2E 1267 listed" },
    });
    expect([201, 409], await madeType.text()).toContain(madeType.status());
    const granted = await request.post("/api/v1/permissions/grants", {
      headers: auth,
      data: { subject_type: "user_type", subject: LISTED_TYPE, capability: "workspace", value: workspaceId, effect: "allow" },
    });
    expect(granted.status(), await granted.text()).toBe(201);
    grantId = (await granted.json()).id;

    // The workspace's OWN restricted bit — never the "workspace" KIND's
    // enforcement switch, which this block never touches and which stays
    // whatever this backend already has it at.
    const restrict = await request.put(`/api/v1/permissions/availability/workspace/${workspaceId}`, {
      headers: auth,
      data: { restricted: true },
    });
    expect(restrict.status(), await restrict.text()).toBe(200);
  });

  test.afterAll(async ({ request }) => {
    if (workspaceId) {
      await request.put(`/api/v1/permissions/availability/workspace/${workspaceId}`, {
        headers: auth,
        data: { restricted: false },
      });
    }
    if (grantId) await request.delete(`/api/v1/permissions/grants/${grantId}`, { headers: auth });
    if (workspaceId) await request.delete(`/api/v1/workspaces/${workspaceId}`, { headers: auth });
  });

  test("a member outside the restriction: Launch is disabled with the canon sentence", async ({ page }) => {
    await page.addInitScript(([key, tok]) => localStorage.setItem(key, tok), [TOKEN_KEY, seedUserTokenRaw("standard")]);
    await gotoConsole(page);
    await navToRoute(page, "/runs/new");
    await page.getByRole("combobox", { name: "Workspace" }).click();
    await page.getByRole("option", { name: "e2e-1267-workspace" }).click();

    const launch = page.getByRole("button", { name: "Launch run" });
    await expect(launch).toBeDisabled();
    await expect(page.getByText(DENIED.WORKSPACE_NOT_AVAILABLE)).toBeVisible();
  });

  test("the listed type: Launch stays enabled, the sentence never renders", async ({ page }) => {
    await page.addInitScript(([key, tok]) => localStorage.setItem(key, tok), [TOKEN_KEY, seedUserTokenRaw(LISTED_TYPE)]);
    await gotoConsole(page);
    await navToRoute(page, "/runs/new");
    await page.getByRole("combobox", { name: "Workspace" }).click();
    await page.getByRole("option", { name: "e2e-1267-workspace" }).click();

    const launch = page.getByRole("button", { name: "Launch run" });
    await expect(launch).toBeEnabled();
    await expect(page.getByText(DENIED.WORKSPACE_NOT_AVAILABLE)).toHaveCount(0);
  });

  test("an admin still sees it available on the Workspaces list", async ({ page }) => {
    await gotoConsole(page);
    await navToRoute(page, "/workspaces");
    const row = page.getByRole("row", { name: /e2e-1267-workspace/ });
    await expect(row).toBeVisible();
    await expect(row.getByText(DENIED.WORKSPACE_NOT_AVAILABLE)).toHaveCount(0);
  });
});
