/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { randomUUID } from "node:crypto";
import {
  test,
  expect,
  ADMIN_TOKEN,
  MEMBER_PRINCIPAL,
  asRealMember,
  asRealSecurityAdmin,
  consoleAPI,
  continueNewRun,
  gotoConsole,
  mockMemberRole,
  navToRoute,
  openNewRun,
  sidebarLink,
  sql,
  startGuided,
  chooseShape,
  openRepositoryPicker,
  repositoryOption,
} from "./fixtures";
import { NR_FILES } from "../src/app/components/wardyn/copy/new-run/files";
import { NR_HARNESSES } from "../src/app/components/wardyn/copy/new-run/harnesses";
import { VIEW_REFUSAL } from "../src/app/components/wardyn/copy/console-view";
import { DENIED } from "../src/app/lib/permissions-copy";

test.describe("member console — real member navigation", () => {
  test("member nav is Runs · Approvals · Workspaces only — no admin-only items", async ({ page }) => {
    await asRealMember(page);
    await gotoConsole(page);

    // Workspaces joined the member set (mock M6): a member launches runs
    // against workspaces, and the New run picker was the only place they were
    // visible at all.
    for (const label of ["Runs", "Approvals", "Workspaces"] as const) {
      await expect(sidebarLink(page, label)).toBeVisible();
    }
    // Permissions joined the sidebar in 0.6 and is admin-only: 0.6 ships no
    // member permissions screen at all, only the inline why-denied moments
    // below, so it must not appear here. Recordings is the admin evidence
    // trail and stays admin-only for the same reason Audit does.
    for (const label of ["Policies", "Governance", "Permissions", "Secrets", "Audit", "Recordings"] as const) {
      await expect(sidebarLink(page, label)).toHaveCount(0);
    }
    // This is the User view, where an admin sees the same nav, so the lines above do not tell a
    // member from an admin. The Admin view does: it refuses a real member.
    await navToRoute(page, "/admin/runs");
    await expect(page.getByRole("heading", { name: VIEW_REFUSAL.TITLE })).toBeVisible();
    await expect(page.getByText(VIEW_REFUSAL.BODY)).toBeVisible();
  });

  test("the account menu shows a quiet 'user' chip", async ({ page }) => {
    await asRealMember(page);
    await gotoConsole(page);

    // The account-menu trigger is the LAST button in the header (after "New
    // run" and "Toggle theme" — see app-shell.tsx's TopBar); its Radix
    // DropdownMenuContent renders role="menu" once opened.
    await page.locator("header").getByRole("button").last().click();
    const menu = page.getByRole("menu");
    // The non-admin side is a user (packet B, D6).
    await expect(menu.getByText("User", { exact: true })).toBeVisible();
  });

  test("admin (unmocked, today's default): the full nine-item nav in the Admin view", async ({ page }) => {
    await gotoConsole(page, "admin");
    for (const label of ["Runs", "Approvals", "Workspaces", "Policies", "Governance", "Permissions", "Secrets", "Audit", "Recordings"] as const) {
      await expect(sidebarLink(page, label)).toBeVisible();
    }
  });
});

test("real member cannot access another person's run", async ({ page }) => {
  await asRealSecurityAdmin(page);
  const foreignCreated = await consoleAPI(page, "POST", "/api/v1/runs", { agent: "claude-code", task: "another person's run" });
  expect(foreignCreated.status, foreignCreated.text).toBe(201);
  const foreignId = JSON.parse(foreignCreated.text).id as string;
  const adminRead = await page.request.get(`/api/v1/runs/${foreignId}`, {
    headers: { Authorization: `Bearer ${ADMIN_TOKEN}` },
  });
  expect(adminRead.status()).toBe(200);

  await asRealMember(page);
  const created = await consoleAPI(page, "POST", "/api/v1/runs", { agent: "claude-code", task: "member-owned run" });
  expect(created.status, created.text).toBe(201);
  const ownId = JSON.parse(created.text).id as string;
  const own = await consoleAPI(page, "GET", `/api/v1/runs/${ownId}`);
  expect(own.status, own.text).toBe(200);
  expect(JSON.parse(own.text)).toMatchObject({ id: ownId, created_by: MEMBER_PRINCIPAL });
  const listed = await consoleAPI(page, "GET", "/api/v1/runs");
  expect(listed.status).toBe(200);
  expect(JSON.parse(listed.text).map((run: { id: string }) => run.id)).toEqual([ownId]);

  const missing = await consoleAPI(page, "GET", `/api/v1/runs/${randomUUID()}`);
  const foreign = await consoleAPI(page, "GET", `/api/v1/runs/${foreignId}`);
  expect(missing.status).toBe(404);
  expect(foreign).toEqual(missing);
  expect((await consoleAPI(page, "POST", `/api/v1/runs/${foreignId}/kill`, {})).status).toBe(404);

  await page.goto(`/runs/${foreignId}`);
  await expect(page.getByText("Run not found")).toBeVisible();
  await expect(page.getByText(/denied|forbidden|no permission|not authorized/i)).toHaveCount(0);
  const foreignCard = await page.getByTestId("refusal").evaluate((el) => el.outerHTML);

  // #3047: the same card, byte for byte, for a run that does not exist.
  await page.goto(`/runs/${randomUUID()}`);
  await expect(page.getByText("Run not found")).toBeVisible();
  expect(await page.getByTestId("refusal").evaluate((el) => el.outerHTML)).toBe(foreignCard);
  await expect(page.getByTestId("refusal").getByRole("button")).toHaveText("Back to your runs");
  await expect(page.getByTestId("refusal")).not.toContainText(foreignId);
});

test.describe("foreign/unknown run — standard not-found (absence, not refusal)", () => {
  test("a well-formed but nonexistent run id renders the SAME generic not-found state a bogus one would", async ({ page }) => {
    // No role mock needed: getRunAuthorized's byte-identical 404 (foreign vs
    // genuinely missing) means this state renders identically for admin and
    // member — this spec proves the CLIENT never special-cases it either way.
    await gotoConsole(page);
    await page.goto("/runs/00000000-0000-0000-0000-000000000000");

    await expect(page.getByText("Run not found")).toBeVisible();
    // run-detail.tsx now also names "you don't have access to it" alongside
    // archived/deleted/stale-link (run-detail.test.tsx: "names lack-of-access
    // as a real reason") — undifferentiated from the other reasons, so the
    // anti-enumeration property this spec is about still holds.
    await expect(
      page.getByText(/this run may have been archived or deleted, the link is stale, or you don't have access to it/i),
    ).toBeVisible();
    // No denial-flavored copy — the product must never confirm the id was
    // ever real (no existence oracle).
    await expect(page.getByText(/denied|forbidden|no permission|not authorized/i)).toHaveCount(0);
    await expect(page.getByRole("button", { name: /back to runs/i })).toBeVisible();
  });
});


// ---------------------------------------------------------------------------
// Render-only stale-group cases: the operator token has no human group
// snapshot. Keep that deliberately incomplete shape separate from real actors.
test.describe.configure({ mode: "serial" });

test.describe("member why-denied (mocked /me role, real enforcement)", () => {
  test.beforeEach(async ({ page }) => {
    await page.request.put("/api/v1/permissions/enforcement", {
      headers: { Authorization: `Bearer ${ADMIN_TOKEN}` },
      data: { egress_host: true, workspace: true },
    });
  });

  test("an ungranted egress host: both decisions disable, with the reason and the Always caveat", async ({ page }) => {
    const runId = sql("SELECT id FROM agent_runs ORDER BY created_at LIMIT 1");
    expect(runId, "no seeded runs found — is the backend up and seeded?").not.toBe("");
    sql(`DELETE FROM approvals WHERE state = 'PENDING'`);
    sql(
      `INSERT INTO approvals (id, run_id, kind, requested_scope, state, requested_at)
       VALUES ('${randomUUID()}', '${runId}', 'egress_domain', '{"host":"registry.npmjs.org"}'::jsonb, 'PENDING', now())`,
    );

    await mockMemberRole(page);
    await gotoConsole(page);
    await navToRoute(page, "/approvals");

    await expect(page.getByRole("button", { name: /^Approve$/ })).toBeDisabled();
    await expect(page.getByRole("button", { name: /^Deny$/ })).toBeDisabled();
    await expect(page.getByText(DENIED.APPROVE_CHIP)).toBeVisible();
    await expect(page.getByText(DENIED.APPROVE_BODY("registry.npmjs.org"))).toBeVisible();
    await expect(page.getByText(DENIED.ALWAYS_STILL_ADMIN)).toBeVisible();
    // A bearer session carries no recorded groups, so the snapshot ceiling is
    // named too — the member's half of the stale-groups story.
    await expect(page.getByText(DENIED.STALE_GROUPS)).toBeVisible();
  });

  test("an ungranted workspace's repository stays LISTED, shut, and says why — visibility is not capability", async ({ page, request }) => {
    // #1267: available_to_you is computed for the REAL caller (decide's own
    // step 1 exempts an operator unconditionally), so a role SPLICE no longer
    // suffices here — mockMemberRole's request still carries the operator
    // bearer, which the server now correctly reports as always available.
    // asRealMember is a genuine non-operator token, so the real backend's
    // real enforcement (this describe's own beforeEach) refuses it for real.
    //
    // The Files step's Add dialog lists repositories, so the ungranted workspace is one with a repository.
    const auth = { Authorization: `Bearer ${ADMIN_TOKEN}` };
    const created = await request.post("/api/v1/workspaces", {
      headers: auth,
      data: { name: "e2e-mc-ungranted", sources: [{ type: "repo", source: "https://dev.azure.com/contoso/nr/_git/e2e-mc-ungranted" }] },
    });
    expect(created.status(), await created.text()).toBe(201);
    const id = ((await created.json()) as { id: string }).id;
    try {
      await asRealMember(page);
      await gotoConsole(page);
      await openNewRun(page);
      await startGuided(page);
      await chooseShape(page, "environment");
      await continueNewRun(page, NR_HARNESSES.TITLE);
      const listed = page.waitForResponse((r) => new URL(r.url()).pathname === "/api/v1/workspaces" && r.request().method() === "GET");
      await continueNewRun(page, NR_FILES.TITLE);
      await listed;

      const picker = await openRepositoryPicker(page);
      await expect(repositoryOption(picker, "e2e-mc-ungranted")).toBeDisabled();
      // #922 review F5: the ONE canon sentence, on the row it is about.
      await expect(picker.getByText(DENIED.WORKSPACE_NOT_AVAILABLE)).toBeVisible();
    } finally {
      await request.delete(`/api/v1/workspaces/${id}`, { headers: auth });
    }
  });
});

// M-5 (#636) moved the "redacted body is not a deployment fact" Settings case
// this describe used to carry — Host left /account entirely, so there is
// nothing left there for a redacted body to leak into (settings-connections
// .spec.ts's "a member's Your account has no Host card at all" is the split's
// replacement). This lone survivor never belonged to that title; it's its own
// now.
test.describe("member console — runs empty state", () => {
  test("an empty board offers the member's own next move, not the operator funnel", async ({ page }) => {
    await asRealMember(page);
    await page.route(/\/api\/v1\/runs(\?|$)/, (route) =>
      route.request().method() === "GET" ? route.fulfill({ json: [] }) : route.continue(),
    );
    await gotoConsole(page);
    await navToRoute(page, "/runs");

    await expect(page.getByText("Runs you launch appear here")).toBeVisible();
    await expect(page.getByText("No runs yet")).toHaveCount(0);
    await expect(page.getByRole("link", { name: "New run" })).toHaveAttribute("href", "/runs/new");
    await expect(page.getByRole("main").getByRole("link", { name: "Getting started" })).toHaveAttribute("href", "/setup");
  });
});
