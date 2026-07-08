/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, gotoConsole, navTo } from "./fixtures";
import type { Page } from "@playwright/test";

// E2E coverage for the Secrets screen
// (src/app/components/screens/secrets.tsx).
//
// The backend serves a SHARED prebuilt ui/dist, so this spec only uses
// accessible selectors that already exist in the built UI (roles/text/labels),
// never CSS classes or test-ids it would have to add.
//
// Backend invariants exercised here:
//  - secrets are WRITE-ONLY: only names are ever returned by the API, the value
//    is never echoed to the DOM anywhere.
//  - the seeded backend ships exactly one secret, "e2e-test-secret".
//  - PUT /secrets/{name} is an upsert (overwrite), so the UI guards an existing
//    name behind a two-step "Save (overwrites)" -> "Overwrite secret" confirm.
//  - DELETE failures must surface as toast.error (the medium fix), not silently.

const SEED_SECRET = "e2e-test-secret";

// Navigate to a freshly-loaded Secrets screen and wait until the table (or empty
// state) has finished its initial load. We key on the page heading + the seeded
// secret name so we know the list query resolved.
async function openSecrets(page: Page) {
  await gotoConsole(page);
  await navTo(page, "Secrets");
  await expect(page.getByRole("heading", { name: "Secrets", exact: true })).toBeVisible();
}

// The Add-secret dialog, scoped so we never accidentally match the page behind
// it. Keyed on the stable write-only DESCRIPTION, not the title — the title
// intentionally flips "Add secret" → "Rotate secret" once the typed name
// matches an existing secret, and the dialog must stay matchable across that.
function addDialog(page: Page) {
  return page.getByRole("dialog").filter({ hasText: "stored write-only" });
}

// Open the Add-secret dialog from the page header action and return it.
async function openAddDialog(page: Page) {
  await page.getByRole("button", { name: "Add secret" }).first().click();
  const dlg = addDialog(page);
  await expect(dlg).toBeVisible();
  return dlg;
}

// Open a row's "…" action menu and activate its single "Delete" item.
//
// The Radix dropdown content is rendered in a portal and, because the table sits
// high in the viewport, it opens UPWARD — placing the menu item above y=0 so a
// positional click reports "outside of the viewport". The menu is keyboard-
// navigable though, so we drive it with the keyboard (ArrowDown -> Enter), which
// is both robust and how a real keyboard user would delete a secret.
async function openRowDeleteMenu(page: Page, secretName: string) {
  const row = page.getByRole("row").filter({ hasText: secretName });
  await expect(row).toBeVisible();
  await row.getByRole("button").last().click();
  // The menu has two items (Rotate, then Delete) — click Delete directly;
  // keyboard-relative navigation proved order/focus fragile here.
  const del = page.getByRole("menuitem", { name: "Delete" });
  await expect(del).toBeVisible();
  await del.click();
}

test.describe("Secrets screen", () => {
  test("lists the seeded secret name and renders the write-only header", async ({ page }) => {
    await openSecrets(page);

    // The page documents the write-only contract.
    await expect(
      page.getByText(/Write-only: values go in and never come out/i)
    ).toBeVisible();

    // The seeded secret name is listed in the table.
    const row = page.getByRole("row").filter({ hasText: SEED_SECRET });
    await expect(row).toBeVisible();

    // The Name column header is present.
    await expect(page.getByRole("columnheader", { name: "Name" })).toBeVisible();

    // The footer count reflects the number of listed secrets. We derive the
    // expected count from the API (rather than hardcoding) so the assertion is
    // robust to any sibling secrets, then assert correct singular/plural copy.
    // Unfiltered, the footer reads "<n> of <n> secret(s)" (filtered of total).
    const list = await page.request.get(`/api/v1/secrets`, {
      headers: { Authorization: "Bearer wardyn-e2e-token" },
    });
    const { names } = (await list.json()) as { names: string[] };
    const n = names.length;
    const expectedCount = `${n} of ${n} secret${n === 1 ? "" : "s"}`;
    await expect(page.getByText(expectedCount, { exact: true })).toBeVisible();
  });

  test("never exposes a secret value anywhere on the screen", async ({ page }) => {
    await openSecrets(page);
    await expect(page.getByRole("row").filter({ hasText: SEED_SECRET })).toBeVisible();

    // The list API only ever returns names. Assert no input/textarea on the
    // screen is pre-populated with a value, and that the only secret-shaped text
    // present is the name itself (write-only contract).
    const body = page.locator("body");
    // The seeded secret's ACTUAL value (set by scripts/e2e-backend.sh) must never
    // appear in the DOM — this is the load-bearing write-only assertion for the
    // seeded secret (the sentinels below are extra guards against common leak
    // shapes, but only this matches the real stored value).
    await expect(body).not.toContainText("e2e-secret-value");
    // A value would have to come from the API, which never returns one — so any
    // common secret-value sentinels must be absent.
    await expect(body).not.toContainText("sk-");
    await expect(body).not.toContainText("BEGIN PRIVATE KEY");

    // The Add dialog's value field starts empty and shows no prior value.
    const dlg = await openAddDialog(page);
    const value = dlg.getByLabel("Value");
    await expect(value).toHaveValue("");
    // Name field also starts empty even though a secret exists.
    await expect(dlg.getByLabel("Name")).toHaveValue("");
  });

  test("search filters the list and shows the no-match empty state", async ({ page }) => {
    await openSecrets(page);
    const search = page.getByPlaceholder("Search secrets by name…");

    // A matching query keeps the seeded secret visible.
    await search.fill("e2e");
    await expect(page.getByRole("row").filter({ hasText: SEED_SECRET })).toBeVisible();

    // A non-matching query renders the search-specific empty state.
    await search.fill("zzz-no-such-secret");
    await expect(page.getByText("No secrets match that search.")).toBeVisible();
    await expect(page.getByRole("button", { name: "Clear search" })).toBeVisible();

    // Clearing the search restores the row.
    await search.fill("");
    await expect(page.getByRole("row").filter({ hasText: SEED_SECRET })).toBeVisible();
  });

  test("validates the secret name before allowing a save", async ({ page }) => {
    await openSecrets(page);
    const dlg = await openAddDialog(page);

    // Uppercase / invalid characters are rejected client-side with an inline
    // error and no round-trip. Fill value too so the Save button is enabled.
    await dlg.getByLabel("Name").fill("Invalid Name");
    await dlg.getByLabel("Value").fill("some-value");
    await dlg.getByRole("button", { name: "Save secret" }).click();
    await expect(dlg.getByText(/Invalid name:/i)).toBeVisible();

    // The dialog stays open after a validation failure.
    await expect(dlg).toBeVisible();
    await dlg.getByRole("button", { name: "Cancel" }).click();
    await expect(addDialog(page)).toBeHidden();
  });

  test("adds a new lowercase secret and it appears in the list", async ({ page }) => {
    const newName = `e2e-added-${Date.now()}`;
    await openSecrets(page);

    const dlg = await openAddDialog(page);
    await dlg.getByLabel("Name").fill(newName);
    await dlg.getByLabel("Value").fill("sk-super-secret-value");

    // A brand-new name is not an overwrite: button reads "Save secret".
    const saveBtn = dlg.getByRole("button", { name: "Save secret" });
    await expect(saveBtn).toBeEnabled();
    await saveBtn.click();

    // Dialog closes on success and the new name appears in the table.
    await expect(addDialog(page)).toBeHidden();
    await expect(page.getByRole("row").filter({ hasText: newName })).toBeVisible();

    // The value we typed is never echoed back into the DOM.
    await expect(page.locator("body")).not.toContainText("sk-super-secret-value");

    // Cleanup so the lane stays re-seed tolerant: delete the secret we added.
    await page.request.delete(`/api/v1/secrets/${encodeURIComponent(newName)}`, {
      headers: { Authorization: "Bearer wardyn-e2e-token" },
    });
  });

  test("typing an existing name shows the overwrite warning and requires a two-step confirm", async ({
    page,
  }) => {
    await openSecrets(page);
    const dlg = await openAddDialog(page);

    // Typing the seeded secret's exact name triggers the overwrite warning.
    await dlg.getByLabel("Name").fill(SEED_SECRET);
    await expect(
      dlg.getByText(/already exists\. Saving will overwrite/i)
    ).toBeVisible();

    // Before any save the button advertises the overwrite via "Save (overwrites)".
    await dlg.getByLabel("Value").fill("new-rotated-value");
    const overwriteSave = dlg.getByRole("button", { name: "Save (overwrites)" });
    await expect(overwriteSave).toBeVisible();

    // FIRST click does NOT save — it flips the button to the explicit
    // "Overwrite secret" confirm and keeps the dialog open.
    await overwriteSave.click();
    const confirmBtn = dlg.getByRole("button", { name: "Overwrite secret", exact: true });
    await expect(confirmBtn).toBeVisible();
    await expect(dlg).toBeVisible();

    // SECOND click actually performs the upsert; dialog closes and the secret
    // remains listed (same name, now overwritten value).
    await confirmBtn.click();
    await expect(addDialog(page)).toBeHidden();
    await expect(page.getByRole("row").filter({ hasText: SEED_SECRET })).toBeVisible();

    // The value is still never shown anywhere.
    await expect(page.locator("body")).not.toContainText("new-rotated-value");
  });

  test("editing the name after acknowledging overwrite resets the confirm step", async ({ page }) => {
    await openSecrets(page);
    const dlg = await openAddDialog(page);

    await dlg.getByLabel("Name").fill(SEED_SECRET);
    await dlg.getByLabel("Value").fill("v1");
    // Advance to the confirm step.
    await dlg.getByRole("button", { name: "Save (overwrites)" }).click();
    await expect(dlg.getByRole("button", { name: "Overwrite secret", exact: true })).toBeVisible();

    // Changing the name to a non-existing one clears the overwrite state: the
    // warning disappears and the button reverts to the plain "Save secret".
    await dlg.getByLabel("Name").fill(`fresh-name-${Date.now()}`);
    await expect(dlg.getByText(/already exists\. Saving will overwrite/i)).toBeHidden();
    await expect(dlg.getByRole("button", { name: "Save secret" })).toBeVisible();

    await dlg.getByRole("button", { name: "Cancel" }).click();
    await expect(addDialog(page)).toBeHidden();
  });

  test("deleting a secret confirms via dialog and shows a success toast", async ({ page }) => {
    // Self-contained: create a throwaway secret to delete so we never remove the
    // shared seeded one (keeps the lane re-seed tolerant).
    const victim = `e2e-del-${Date.now()}`;
    const resp = await page.request.put(`/api/v1/secrets/${encodeURIComponent(victim)}`, {
      headers: { Authorization: "Bearer wardyn-e2e-token", "Content-Type": "application/json" },
      data: { value: "to-be-deleted" },
    });
    expect(resp.ok()).toBeTruthy();

    await openSecrets(page);

    // Open the row's action menu and choose Delete.
    await openRowDeleteMenu(page, victim);

    // The confirm AlertDialog names the secret being deleted.
    const confirm = page.getByRole("alertdialog");
    await expect(confirm).toBeVisible();
    await expect(confirm.getByText(new RegExp(`Delete secret .*${victim}`))).toBeVisible();

    // Confirm the deletion.
    await confirm.getByRole("button", { name: "Delete secret" }).click();

    // Success toast appears and the row disappears.
    await expect(page.getByText(/deleted/i).first()).toBeVisible();
    await expect(page.getByRole("row").filter({ hasText: victim })).toHaveCount(0);
  });

  test("a delete FAILURE surfaces toast.error and keeps the secret (the medium fix)", async ({
    page,
  }) => {
    // Create a throwaway secret, then force its DELETE to fail server-side via a
    // route intercept. The medium fix must surface this as toast.error rather
    // than silently pretending the secret is gone.
    const victim = `e2e-delfail-${Date.now()}`;
    const resp = await page.request.put(`/api/v1/secrets/${encodeURIComponent(victim)}`, {
      headers: { Authorization: "Bearer wardyn-e2e-token", "Content-Type": "application/json" },
      data: { value: "stays-put" },
    });
    expect(resp.ok()).toBeTruthy();

    await openSecrets(page);
    await expect(page.getByRole("row").filter({ hasText: victim })).toBeVisible();

    // Intercept the DELETE for THIS secret and return a 500 so confirmDelete()
    // hits its catch branch.
    await page.route(`**/api/v1/secrets/${encodeURIComponent(victim)}`, async (route) => {
      if (route.request().method() === "DELETE") {
        await route.fulfill({ status: 500, contentType: "text/plain", body: "boom" });
      } else {
        await route.continue();
      }
    });

    await openRowDeleteMenu(page, victim);

    const confirm = page.getByRole("alertdialog");
    await expect(confirm).toBeVisible();
    await confirm.getByRole("button", { name: "Delete secret" }).click();

    // The failure is surfaced as a toast.error (NOT silent). The toast title
    // names the secret (the description carries the server's "HTTP 500" detail),
    // so we key on the title text + the victim name to pin the title node.
    await expect(
      page.getByText(new RegExp(`Failed to delete secret .*${victim}`))
    ).toBeVisible();
    // The toast also surfaces the underlying server error as its description.
    await expect(page.getByText(/HTTP 500/)).toBeVisible();
    // ... the confirm dialog stays open so the operator can retry ...
    await expect(confirm).toBeVisible();

    // ... and the secret is still really present (verified out-of-band so the
    // intercept doesn't mask the truth).
    await page.unroute(`**/api/v1/secrets/${encodeURIComponent(victim)}`);
    const list = await page.request.get(`/api/v1/secrets`, {
      headers: { Authorization: "Bearer wardyn-e2e-token" },
    });
    const body = (await list.json()) as { names: string[] };
    expect(body.names).toContain(victim);

    // Cleanup: now actually delete it (intercept removed).
    await page.request.delete(`/api/v1/secrets/${encodeURIComponent(victim)}`, {
      headers: { Authorization: "Bearer wardyn-e2e-token" },
    });
  });
});
