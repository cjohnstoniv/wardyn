/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, sql, consoleAPI, expandCard, asRealMember, asRealSecurityAdmin } from "./fixtures";
import type { Page } from "@playwright/test";

// ============================================================================
// Preview URLs (docs/UI-SANDBOXES.md, "Preview URLs") against the SEEDED backend: an admin turns them on in
// Settings; the run's owner creates a private preview of a port and opens it, which leaves the console for the
// gateway; a second signed-in person is told it has ended, and the ticket route answers them 404.
//
// The `none` runner behind this backend has no sandbox, so nothing listens on the port. The cases here are the
// ones that need no listener: who gets a ticket, what the console says, and that the owner's hand-off goes to the
// gateway's own origin, as ui-sandbox.spec.ts does for a declared app. A preview served for real is proven live by
// scripts/run-e2e-ui-sandbox.sh. The states run in order on one backend: the switch, then the preview.
// ============================================================================

test.describe.configure({ mode: "serial" });

const RUN_TASK = "e2e fixture 2";
const PORT = 3000;
const REVOKED_LINE = "This preview has ended or was revoked. Ask the person who shared it for a new link.";
const ADMIN_LINE = "You can see and revoke the previews on this run. Only the run's owner creates them.";

function runId(): string {
  const id = sql(`SELECT id FROM agent_runs WHERE task = '${RUN_TASK}' LIMIT 1`);
  expect(id, `no seeded run with task ${RUN_TASK}`).toMatch(/^[0-9a-f-]{36}$/);
  return id;
}

async function openRun(page: Page): Promise<void> {
  await page.goto(`/runs/${runId()}`);
  await expect(page.getByRole("heading", { name: RUN_TASK, level: 1 })).toBeVisible();
}

test.describe("Preview URLs", () => {
  test("an admin turns previews on in Settings, and the card keeps what it saved", async ({ page }) => {
    await page.goto("/admin/settings");
    await expandCard(page, "Preview URLs");
    const card = page.getByTestId("preview-urls-card");
    const toggle = card.getByRole("switch", { name: "Allow preview URLs" });
    await expect(toggle).not.toBeChecked();
    // The ceiling is dimmed while the switch is off.
    await expect(card.getByRole("radio", { name: "Private" })).toBeDisabled();
    await toggle.click();
    await expect(card.getByRole("radio", { name: "Private" })).toBeChecked();
    const saved = page.waitForResponse((r) => r.url().includes("/api/v1/site-config") && r.request().method() === "PUT");
    await card.getByRole("button", { name: "Save" }).click();
    const response = await saved;
    expect(response.ok(), await response.text()).toBe(true);
    await expect(card.getByRole("button", { name: "Save" })).toBeDisabled();
    await page.reload();
    await expandCard(page, "Preview URLs");
    await expect(page.getByTestId("preview-urls-card").getByRole("switch", { name: "Allow preview URLs" })).toBeChecked();
  });

  test("the owner creates a private preview of a port and opens it, which leaves the console for the gateway", async ({ page, context }) => {
    await openRun(page);
    const tile = page.getByTestId("previews-panel");
    await tile.scrollIntoViewIfNeeded();
    await expect(tile.getByText("No previews yet.")).toBeVisible();
    // Private is preselected, and Public is dimmed: the ceiling this card saved is Private, so Organisation is too.
    await expect(tile.getByRole("radio", { name: "Private" })).toBeChecked();
    await expect(tile.getByRole("radio", { name: "Public" })).toBeDisabled();
    await tile.getByLabel("Port", { exact: true }).fill(String(PORT));
    await tile.getByRole("button", { name: "Create preview" }).click();
    const row = tile.getByTestId(`preview-${PORT}`);
    await expect(row).toContainText(`Port ${PORT}`);
    await expect(row).toContainText("Private");
    // relativeTime reads a day as 1d once the 24 hours are whole; either way it is an "in" time.
    await expect(row).toContainText(/Expires in (24h|1d)/);
    expect(await page.locator("iframe").count()).toBe(0);

    // Open takes the ticket and POSTs it into a new tab at the gateway's own origin; the request is stopped there,
    // since there is no sandbox to relay into.
    let captured: { url: string; postData: string | null } | null = null;
    await context.route("**/__wardyn/enter", async (route) => {
      captured = { url: route.request().url(), postData: route.request().postData() };
      await route.fulfill({ status: 200, contentType: "text/plain", body: "stopped for the test" });
    });
    const mint = page.waitForResponse((r) => r.url().includes(`/runs/${runId()}/previews/${PORT}/ticket`) && r.request().method() === "POST");
    const [popup] = await Promise.all([context.waitForEvent("page"), row.getByRole("button", { name: "Open" }).click()]);
    expect((await mint).status()).toBe(200);
    await popup.waitForLoadState("domcontentloaded").catch(() => {});
    await expect.poll(() => captured).not.toBeNull();
    const { url, postData } = captured!;
    expect(new URL(url).origin).not.toBe(new URL(page.url()).origin);
    expect(new URL(url).search).toBe("");
    const form = new URLSearchParams(postData ?? "");
    expect(form.get("run")).toBe(runId());
    expect(form.get("app")).toBe(`__preview-${PORT}`);
    expect((form.get("ticket") ?? "").length).toBeGreaterThan(16);
    await popup.close();
  });

  test("a second signed-in person gets 404 for the preview's ticket, and the console route says it has ended", async ({ page }) => {
    await asRealMember(page);
    const ticket = await consoleAPI(page, "POST", `/api/v1/runs/${runId()}/previews/${PORT}/ticket`);
    expect(ticket.status, ticket.text).toBe(404);
    expect(JSON.parse(ticket.text).reason).toBe("run_not_found");
    // The same 404 for a person it does not admit and for a preview that is gone: the page cannot tell them apart.
    await page.goto(`/previews/${runId()}/${PORT}`);
    const alert = page.getByRole("alert");
    await expect(alert).toContainText(`Couldn't open port ${PORT}`);
    await expect(alert).toContainText(REVOKED_LINE);
    const none = await consoleAPI(page, "POST", `/api/v1/runs/${runId()}/previews/${PORT + 1}/ticket`);
    expect(none.status, none.text).toBe(404);
  });

  test("a security admin on the run sees the preview, with no form, and revokes it in one click", async ({ page }) => {
    await asRealSecurityAdmin(page);
    await openRun(page);
    const tile = page.getByTestId("previews-panel");
    await tile.scrollIntoViewIfNeeded();
    await expect(tile.getByText(ADMIN_LINE)).toBeVisible();
    const row = tile.getByTestId(`preview-${PORT}`);
    await expect(row).toContainText(`Port ${PORT}`);
    // A private preview opens for the run's entry rule only: no Open for the admin, and no form to create one.
    await expect(row.getByRole("button", { name: "Open" })).toHaveCount(0);
    await expect(tile.getByRole("button", { name: "Create preview" })).toHaveCount(0);
    await expect(tile.getByRole("button", { name: "New preview" })).toHaveCount(0);
    await row.getByRole("button", { name: "Revoke" }).click();
    await expect(row).toContainText("Revoked");
    await expect(row.getByRole("button")).toHaveCount(0);
    const list = await consoleAPI(page, "GET", `/api/v1/runs/${runId()}/previews`);
    expect(list.status, list.text).toBe(200);
    expect(JSON.parse(list.text).previews).toEqual([]);
  });
});
