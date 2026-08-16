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
import { test, expect, gotoConsole } from "./fixtures";
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
});
