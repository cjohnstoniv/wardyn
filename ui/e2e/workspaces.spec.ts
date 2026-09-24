/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Stage 2 of the UI refactor collapsed the 3-tab Workspaces screen + 7-step
// "Add workspace" wizard (workspace-wizard.spec.ts) into a single table +
// one dialog. This file replaces that spec: the four-column list, opening
// the new dialog, its validation (a real defect the mock review caught —
// placeholders must never become values), and that a submit actually
// produces a usable row. Kept small and honest — it does not assert on
// anything this stage didn't build (no scan/build progress, no Sources tab).
import { test, expect, ADMIN_TOKEN, gotoConsole, mockMemberRole } from "./fixtures";
import type { Page } from "@playwright/test";

function uniqueName(tag: string): string {
  return `ws-${tag}-${Date.now()}-${Math.floor(Math.random() * 1000)}`;
}

async function openAddWorkspaceDialog(page: Page) {
  await gotoConsole(page);
  await page.goto("/workspaces");
  await page.getByRole("button", { name: "Add workspace" }).click();
  const dlg = page.getByRole("dialog");
  await expect(dlg.getByRole("heading", { name: "Add workspace" })).toBeVisible();
  return dlg;
}

test.describe("Workspaces — the four-column list", () => {
  test("shows Workspace / Source / Image / Model + the seeded fixture's row", async ({ page }) => {
    await gotoConsole(page);
    await page.goto("/workspaces");

    for (const col of ["Workspace", "Source", "Image", "Model"]) {
      await expect(page.getByRole("columnheader", { name: col })).toBeVisible();
    }
    // No Status column, no "Needs you" column — Stage 2 dropped both.
    await expect(page.getByRole("columnheader", { name: "Status" })).not.toBeVisible();
    await expect(page.getByRole("columnheader", { name: "Needs you" })).not.toBeVisible();

    // scripts/e2e-backend.sh seeds a "payments" local_dir workspace with no
    // base_image and no llm_cred — the honest degenerate case for the Image
    // and Model columns (standard sandbox image; unbound reads a bare "—").
    const row = page.getByRole("row", { name: /payments/ });
    await expect(row).toBeVisible();
    await expect(row.getByText("/home/me/projects/payments")).toBeVisible();
    await expect(row.getByText("standard sandbox image")).toBeVisible();
    await expect(row.getByText("—")).toBeVisible();
  });
});

test.describe("Add workspace dialog", () => {
  test("Add workspace is disabled until a Repository URL is entered", async ({ page }) => {
    const dlg = await openAddWorkspaceDialog(page);
    const add = dlg.getByRole("button", { name: "Add workspace" });
    // Repository is the default segment.
    await expect(dlg.getByLabel("Repository URL")).toBeVisible();
    await expect(add).toBeDisabled();

    await dlg.getByLabel("Repository URL").fill("acme/payments-service");
    await expect(add).toBeEnabled();

    await dlg.getByRole("button", { name: "Cancel" }).click();
    await expect(page.getByRole("dialog")).not.toBeVisible();
  });

  test("Local directory mode gates the same way — a Path is required", async ({ page }) => {
    const dlg = await openAddWorkspaceDialog(page);
    await dlg.getByRole("button", { name: "Local directory" }).click();
    const add = dlg.getByRole("button", { name: "Add workspace" });
    await expect(dlg.getByLabel("Path on this host")).toBeVisible();
    await expect(add).toBeDisabled();

    await dlg.getByLabel("Path on this host").fill("/home/me/projects/reports");
    await expect(add).toBeEnabled();

    await dlg.getByRole("button", { name: "Cancel" }).click();
  });

  test("Empty mode needs nothing typed — Add stays enabled, one click", async ({ page }) => {
    const dlg = await openAddWorkspaceDialog(page);
    await dlg.getByRole("button", { name: "Empty" }).click();
    await expect(dlg.getByText(/empty scratch directory/i)).toBeVisible();
    await expect(dlg.getByRole("button", { name: "Add workspace" })).toBeEnabled();

    await dlg.getByRole("button", { name: "Cancel" }).click();
  });

  test("submitting appends a usable row — no scan, no build, straight to the new workspace's page", async ({
    page,
  }) => {
    const name = uniqueName("submit");
    const dlg = await openAddWorkspaceDialog(page);

    await dlg.getByRole("button", { name: "Local directory" }).click();
    await dlg.getByLabel("Path on this host").fill("/home/me/projects/reports");
    await dlg.getByLabel("Name").fill(name);

    await dlg.getByRole("button", { name: "Add workspace" }).click();

    // One POST /workspaces later, straight to the detail page — no
    // intermediate scanning/building state to wait through.
    await expect(page.getByRole("dialog")).not.toBeVisible();
    await expect(page).toHaveURL(/\/workspaces\/[^/]+$/);
    await expect(page.getByRole("heading", { name })).toBeVisible();
    await expect(page.getByRole("button", { name: /^start a run$/i })).toBeEnabled();

    // ...and it's really in the list, not just a one-off detail render.
    await page.goto("/workspaces");
    await expect(page.getByRole("row", { name: new RegExp(name) })).toBeVisible();
  });

  // #485: Azure DevOps project and repository names carry spaces and most
  // punctuation. The URL typed with its spaces is accepted, the Name pre-fills
  // with the repository's own name (never its escapes), and the server stores
  // the one canonical spelling the detail page then shows.
  test("an Azure DevOps repository with spaces in its names is added as typed, named after the repository", async ({
    page,
  }) => {
    const typed = "https://dev.azure.com/contoso/Payments Platform/_git/Card Auth (v2).Service";
    const stored = "https://dev.azure.com/contoso/Payments%20Platform/_git/Card%20Auth%20(v2).Service";
    const dlg = await openAddWorkspaceDialog(page);
    await dlg.getByLabel("Repository URL").fill(typed);
    await expect(dlg.getByLabel("Name")).toHaveValue("Card Auth (v2).Service");

    const created = page.waitForResponse(
      (r) => new URL(r.url()).pathname === "/api/v1/workspaces" && r.request().method() === "POST",
    );
    await dlg.getByRole("button", { name: "Add workspace" }).click();
    const resp = await created;
    expect(resp.status(), await resp.text()).toBeLessThan(300);
    expect((await resp.json()).sources[0].source).toBe(stored);

    await expect(page).toHaveURL(/\/workspaces\/[^/]+$/);
    await expect(page.getByRole("heading", { name: "Card Auth (v2).Service" })).toBeVisible();
    await expect(page.getByText(stored).first()).toBeVisible();
    await expect(page.getByRole("button", { name: /^start a run$/i })).toBeEnabled();
  });

  // F3-F8: the named 400x640 repro PASSED with Advanced collapsed (~560px
  // content) — it only reproduces once Advanced is expanded (~910px), which
  // this test does. ui/dialog.tsx's primitive-level max-h-[calc(100dvh-2rem)]
  // + overflow-y-auto (this lane) is what keeps Add workspace reachable here.
  test("F3-F8: 400x640, Advanced expanded — Add workspace stays reachable, not clipped off-screen", async ({
    page,
  }) => {
    // gotoConsole waits on the DESKTOP sidebar link — below md that aside is
    // hidden entirely (md:flex), so the viewport switch has to come AFTER
    // landing, not before.
    const dlg = await openAddWorkspaceDialog(page);
    await page.setViewportSize({ width: 400, height: 640 });
    await dlg.getByRole("button", { name: "Local directory" }).click();
    await dlg.getByLabel("Path on this host").fill("/home/me/projects/reports");

    const advanced = dlg.getByRole("button", { name: "Advanced" });
    await expect(advanced).toBeVisible();
    await advanced.click();
    await expect(advanced).toHaveAttribute("aria-expanded", "true");

    const add = dlg.getByRole("button", { name: "Add workspace" });
    await expect(add).toBeVisible();
    // The defect this pins is "no scroll container exists to REACH it" —
    // not "it fits without scrolling" (the dialog is correctly taller than
    // the viewport here; that's what overflow-y-auto is for). Scroll to it
    // first, then prove the scroll actually landed it on-screen.
    await add.scrollIntoViewIfNeeded();
    const box = await add.boundingBox();
    expect(box, "Add workspace boundingBox").not.toBeNull();
    expect(box!.y, "Add workspace top edge").toBeGreaterThanOrEqual(0);
    expect(box!.y + box!.height, "Add workspace bottom edge").toBeLessThanOrEqual(640);
    await expect(add).toBeEnabled();
    await add.click();
    await expect(page.getByRole("dialog")).not.toBeVisible();
  });
});

// X3-F2 / F5-F1 / X2-F4 — DELETE /workspaces/{id} is classOwner server-side: a
// member may delete a workspace THEY created, and the console parked it on the
// admin role for everyone. The harness backend is always an admin (fixtures.ts),
// so the role is spliced the usual way and the OWNER is spliced onto the list
// the console reads — that pair is exactly the member shape this fix is about.
test.describe("Workspaces — a member deletes the row they own", () => {
  test.afterEach(async ({ page }) => {
    // #469 (CI-flake): the list's poll can still be inside the route handler
    // below when the test ends, and closing the context disposes the response
    // it is reading ("Response has been disposed"). Same teardown as
    // setup-gate.spec.ts and people-access.spec.ts.
    await page.unrouteAll({ behavior: "ignoreErrors" });
  });

  test("their own row's Delete is live and lands; an operator-owned row stays parked", async ({ page }) => {
    const mine = uniqueName("mine");
    const created = await page.request.post("/api/v1/workspaces", {
      headers: { Authorization: `Bearer ${ADMIN_TOKEN}` },
      // NOT the seeded "payments" path: a row's accessible name carries its
      // source, so sharing it would make the two row locators below ambiguous.
      data: { name: mine, kind: "local_dir", source: "/home/me/projects/reports" },
    });
    expect(created.ok(), "seeding the member-owned workspace failed").toBeTruthy();

    const me = await (await page.request.get("/api/v1/me", {
      headers: { Authorization: `Bearer ${ADMIN_TOKEN}` },
    })).json();
    const principal: string = me.principal;
    expect(principal, "the harness principal must be non-empty for the owner leg to mean anything").not.toBe("");

    await mockMemberRole(page);
    // Only the row this test created is theirs; every other seeded row keeps
    // the absent owner an operator-created row arrives with.
    await page.route(/\/api\/v1\/workspaces(\?|$)/, async (route) => {
      if (route.request().method() !== "GET") return route.continue();
      const response = await route.fetch();
      const json = await response.json();
      const rows = Array.isArray(json) ? json : (json.workspaces ?? []);
      for (const w of rows) if (w.name === mine) w.owned_by = principal;
      await route.fulfill({ response, json });
    });

    await gotoConsole(page);
    await page.goto("/workspaces");

    // The seeded "payments" row is operator-owned: its Delete is parked, and
    // the reason is stated in the item itself, not on hover.
    const theirs = page.getByRole("row", { name: /payments/ });
    await theirs.getByRole("button", { name: "Workspace actions" }).click();
    await expect(page.getByRole("menuitem", { name: /delete/i })).toHaveAttribute("aria-disabled", "true");
    await page.keyboard.press("Escape");

    const ownRow = page.getByRole("row", { name: new RegExp(mine) });
    await ownRow.getByRole("button", { name: "Workspace actions" }).click();
    const item = page.getByRole("menuitem", { name: /delete/i });
    await expect(item).not.toHaveAttribute("aria-disabled", "true");
    await item.click();

    const confirm = page.getByRole("button", { name: /delete workspace/i });
    await expect(confirm).toBeEnabled();
    await confirm.click();

    // It really went: the row is gone from the re-read list, not just the dialog.
    await expect(page.getByRole("row", { name: new RegExp(mine) })).toHaveCount(0);
  });
});
