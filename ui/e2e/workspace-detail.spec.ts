/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, ADMIN_TOKEN, gotoConsole, navToRoute } from "./fixtures";
import type { Page } from "@playwright/test";

// E2E coverage for the workspace DETAIL page (src/app/components/screens/
// workspace-detail/workspace-detail.tsx) — X2-F5/F4/F19.
//
// record-loop.spec.ts and workspace-egress-tiers.spec.ts both drive this
// screen against a SYNTHETIC workspace id with the GET fully intercepted, so
// neither ever loads the seeded workspace's REAL detail page — a wire-shape
// drift in GET /workspaces/{id} would break every real detail page green
// with nothing here to catch it. This file is the one non-intercepted visit.
//
// It also pins the member delete gate (X2-F4, the UI half — the Go half is
// authz_test.go:474 classOwner / workspaces.test.tsx:307): DELETE
// /workspaces/{id} is owner-or-admin server-side, and useCanMutate(ws.owned_by)
// is supposed to mirror that exactly. The harness bearer is always admin
// server-side (fixtures.ts), so a member's session is spliced onto BOTH GET
// /api/v1/me (role/principal) and GET /api/v1/workspaces/{id} (owned_by) —
// the same route.fetch()+patch+refulfill technique fixtures.ts's
// mockMemberRole documents — to prove the RENDER a member role and a real
// ownership fact together drive, without needing a genuine member session.

const auth = { Authorization: `Bearer ${ADMIN_TOKEN}` };

function uniqueName(tag: string): string {
  return `e2e-wsdetail-${tag}-${Date.now()}`;
}

async function createWorkspace(page: Page, name: string): Promise<string> {
  const res = await page.request.post("/api/v1/workspaces", {
    headers: auth,
    data: { name, sources: [{ type: "local_dir", path: `/tmp/${name}` }] },
  });
  expect(res.ok(), `POST /workspaces for ${name}`).toBe(true);
  const ws = await res.json();
  return ws.id as string;
}

test.describe("Workspace detail — the seeded workspace's real page (X2-F5)", () => {
  test("payments renders its real name, source line, and Start-a-run CTA with no route intercept", async ({
    page,
  }) => {
    const listRes = await page.request.get("/api/v1/workspaces?limit=1000", { headers: auth });
    const workspaces = await listRes.json();
    const payments = (workspaces as Array<{ id: string; name: string }>).find((w) => w.name === "payments");
    expect(payments, "seeded 'payments' workspace").toBeTruthy();

    await gotoConsole(page);
    await navToRoute(page, `/workspaces/${payments!.id}`);

    await expect(page.getByRole("heading", { name: "payments", level: 1 })).toBeVisible();
    // detailSourceLine: kind label + the local_dir path this workspace was
    // seeded with (scripts/e2e-backend.sh) — no ref, so no third segment.
    await expect(page.getByText("local dir · /home/me/projects/payments")).toBeVisible();
    await expect(page.getByRole("button", { name: "Start a run" })).toBeEnabled();
    await expect(page.getByText("Recorded sessions")).toBeVisible();
    await expect(page.getByText("Allowed hosts")).toBeVisible();
    await expect(page.getByText("Denied hosts")).toBeVisible();
  });
});

test.describe("Workspace detail — the member delete gate (X2-F4)", () => {
  test("Delete is enabled for a member's OWN workspace, and parked for another member's", async ({ page }) => {
    const name = uniqueName("owner-gate");
    const wsId = await createWorkspace(page, name);
    const OWNER = "alice@corp.example";
    const OTHER = "bob@corp.example";

    let principal = OWNER;
    await page.route("**/api/v1/me", async (route) => {
      const response = await route.fetch();
      const json = await response.json();
      json.role = "member";
      json.operator = false;
      json.security_operator = false;
      json.principal = principal;
      await route.fulfill({ response, json });
    });
    // Splice owned_by onto the REAL workspace body — everything else (name,
    // source, sessions) stays the genuine server response.
    await page.route(`**/api/v1/workspaces/${wsId}`, async (route) => {
      if (route.request().method() !== "GET") return route.continue();
      const response = await route.fetch();
      const json = await response.json();
      json.owned_by = OWNER;
      await route.fulfill({ response, json });
    });

    await gotoConsole(page);
    await navToRoute(page, `/workspaces/${wsId}`);
    await expect(page.getByRole("heading", { name, level: 1 })).toBeVisible();

    // Scoped to the delete button's OWN hint, not the page: the sibling
    // cards (RecordPane etc.) carry their own useOperator() gates and hints,
    // which a member session trips regardless of workspace ownership — this
    // test is about the ownership-aware Delete gate specifically.
    const deleteBtn = page.getByRole("button", { name: "Delete this workspace" });
    await expect(deleteBtn).toBeEnabled();
    await expect(deleteBtn.getByText("Requires the admin role.")).toHaveCount(0);

    // Same row, a DIFFERENT member — not its owner.
    principal = OTHER;
    await page.reload();
    await expect(page.getByRole("heading", { name, level: 1 })).toBeVisible();
    await expect(deleteBtn).toBeDisabled();
    await expect(deleteBtn.getByText("Requires the admin role.")).toBeVisible();
  });
});

test.describe("Workspace detail — delete round-trip (X2-F19)", () => {
  test("Delete returns to the list, and GET /workspaces confirms it's gone", async ({ page }) => {
    const name = uniqueName("delete-target");
    const wsId = await createWorkspace(page, name);

    await gotoConsole(page);
    await navToRoute(page, `/workspaces/${wsId}`);
    await expect(page.getByRole("heading", { name, level: 1 })).toBeVisible();

    await page.getByRole("button", { name: "Delete this workspace" }).click();
    const confirm = page.getByRole("alertdialog");
    // The dialog's quotes are typographic (“ ”) — match around them, not the
    // character, same as secrets.spec.ts's own delete-confirm pattern.
    await expect(confirm.getByText(new RegExp(`Delete workspace .*${name}`))).toBeVisible();
    await confirm.getByRole("button", { name: "Delete workspace" }).click();
    await expect(confirm).toHaveCount(0);

    await expect(page).toHaveURL(/\/workspaces$/);
    await expect(page.getByRole("row", { name: new RegExp(name) })).toHaveCount(0);

    const res = await page.request.get("/api/v1/workspaces?limit=1000", { headers: auth });
    const names = (await res.json() as Array<{ name: string }>).map((w) => w.name);
    expect(names).not.toContain(name);
  });
});
