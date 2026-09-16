/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, ADMIN_TOKEN, gotoConsole, navToRoute } from "./fixtures";

// E2E coverage for /ssh-keys (src/app/components/screens/ssh-keys.tsx) —
// X2-F2: this screen had ZERO e2e; its own component test
// (ssh-keys.test.tsx) mocks the api module wholesale, so nothing proved the
// real round trip against the seeded backend. GET/POST/DELETE
// /api/v1/me/ssh-keys are self-service (server-side, keyed off the caller's
// own principal) — the bearer-token harness is always the same principal, so
// this spec exercises the real add/list/delete wiring end to end.
//
// A real, parseable ed25519 public key (server-side sshkeys.go rejects a
// private key or anything unparseable, 422) — the exact fixture
// internal/api/sshkeys_test.go already uses, so this key is known-good
// against the server's own key-parsing logic.
const PUBLIC_KEY =
  "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBl3jvXfmZbBd3q5aLKZTv3rIcvKlfz2eYQpuYSGfCPT alice@laptop";
const KEY_NAME = "e2e-laptop";

const auth = { Authorization: `Bearer ${ADMIN_TOKEN}` };

test.describe("SSH keys — add, reload, delete (X2-F2)", () => {
  test("a key added here persists across a reload and is truly gone after delete", async ({ page }) => {
    await gotoConsole(page);
    await navToRoute(page, "/ssh-keys");
    await expect(page.getByRole("heading", { name: "Your SSH keys", level: 1 })).toBeVisible();

    await page.getByRole("button", { name: "Add key" }).first().click();
    const addDialog = page.getByRole("dialog");
    await expect(addDialog.getByRole("heading", { name: "Add key" })).toBeVisible();
    await addDialog.getByLabel("Name").fill(KEY_NAME);
    await addDialog.getByLabel("Public key").fill(PUBLIC_KEY);
    await addDialog.getByRole("button", { name: "Add key" }).click();
    await expect(addDialog).toHaveCount(0);

    const row = page.getByRole("row").filter({ hasText: KEY_NAME });
    await expect(row).toBeVisible();
    const fingerprintCell = row.locator("td").nth(1);
    const fingerprint = (await fingerprintCell.textContent())?.trim() ?? "";
    expect(fingerprint).toMatch(/^SHA256:/);

    // Reload: proves the key was actually STORED server-side, not merely
    // held in the dialog's onAdded() client-side reload of the same load().
    await page.reload();
    await expect(page.getByRole("heading", { name: "Your SSH keys", level: 1 })).toBeVisible();
    await expect(page.getByRole("row").filter({ hasText: KEY_NAME })).toBeVisible();

    // Delete — the icon button in the row, then the confirm dialog. The
    // title's quotes are typographic (“ ”), so match around them rather than
    // hardcode the character (secrets.spec.ts's own confirm-dialog pattern).
    await page.getByRole("button", { name: `Remove key ${KEY_NAME}` }).click();
    const confirmDialog = page.getByRole("alertdialog");
    await expect(confirmDialog.getByText(new RegExp(`Remove key .*${KEY_NAME}`))).toBeVisible();
    await confirmDialog.getByRole("button", { name: "Remove key" }).click();
    await expect(confirmDialog).toHaveCount(0);
    await expect(page.getByRole("row").filter({ hasText: KEY_NAME })).toHaveCount(0);

    // Out-of-band GET — the UI's own reload could theoretically be stale;
    // this asks the server directly, bypassing the screen entirely.
    const res = await page.request.get("/api/v1/me/ssh-keys", { headers: auth });
    expect(res.ok()).toBe(true);
    const keys: Array<{ fingerprint: string; name?: string }> = await res.json();
    expect(keys.some((k) => k.fingerprint === fingerprint || k.name === KEY_NAME)).toBe(false);
  });
});
