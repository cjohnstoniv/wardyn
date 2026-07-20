/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, gotoConsole, navTo } from "./fixtures";
import type { Page } from "@playwright/test";

// Run this file's tests SERIALLY. They share one backend and the policy table is
// global state, so several specs assert on the empty state ("No policies yet")
// which only holds when no other spec has a live policy. Each mutating spec
// creates AND deletes its own policy, so serial execution guarantees every spec
// starts from (and the file leaves behind) the clean empty state — keeping the
// lane fully re-run / re-seed tolerant.
test.describe.configure({ mode: "serial" });

// E2E coverage for the Policies screen (src/app/components/screens/policies.tsx).
//
// Backend reality (verified against the seeded test backend): the demo policy
// loaded via wardynd's -default-policy flag is the in-memory DEFAULT used for runs
// that name no policy — it is NOT persisted as a row, so GET /api/v1/policies
// returns an EMPTY list on a fresh seed. The screen therefore renders its empty
// state by default, and operators populate the table via the create flow. These
// specs cover: the empty state, the create form (client- + server-side
// validation), a full create -> appears -> view spec fields -> delete round-trip,
// and the search/filter "no matching" empty state. Every mutating spec cleans up
// after itself so the lane stays re-run / re-seed tolerant.

// ---- helpers -------------------------------------------------------------

// The page header renders the screen title; the empty state renders an <h3>.
// Scope dialogs by their accessible title because the create editor (Dialog) and
// the view sheet (Sheet) are both Radix dialogs with role="dialog".
function editorDialog(page: Page) {
  return page.getByRole("dialog").filter({ hasText: "New policy" });
}
function editEditorDialog(page: Page) {
  return page.getByRole("dialog").filter({ hasText: "Edit policy" });
}

// Open the create editor from the page-header action button.
async function openCreate(page: Page) {
  // Two "New policy" buttons can exist (header + empty-state action). The header
  // one is always present; pick the first.
  await page.getByRole("button", { name: "New policy" }).first().click();
  await expect(editorDialog(page)).toBeVisible();
}

// Fill the create form. specJson is written verbatim into the JSON textarea.
async function fillEditor(page: Page, name: string, specJson: string) {
  const dialog = editorDialog(page);
  await dialog.getByLabel("Name").fill(name);
  await dialog.getByLabel("Spec (JSON)").fill(specJson);
}

const VALID_SPEC = JSON.stringify(
  {
    allowed_domains: ["api.anthropic.com", "github.com"],
    denied_domains: ["evil.example.com"],
    first_use_approval: true,
    min_confinement_class: "CC2",
    eligible_grants: [
      { kind: "github_token", requires_approval: true, ttl_seconds: 3600 },
    ],
  },
  null,
  2,
);

// Create a policy through the UI and wait for its row to appear; returns nothing.
async function createPolicyViaUi(page: Page, name: string, specJson = VALID_SPEC) {
  await openCreate(page);
  await fillEditor(page, name, specJson);
  await editorDialog(page).getByRole("button", { name: "Create policy" }).click();
  await expect(editorDialog(page)).toBeHidden();
  await expect(policyRow(page, name)).toBeVisible();
}

// A table row scoped by the policy name (Name is the first cell).
function policyRow(page: Page, name: string) {
  // Data rows are clickable <TableRow role="button"> (policies.tsx), not
  // role="row"; match the button carrying the name, scoped to the table.
  return page.getByRole("table").getByRole("button").filter({ hasText: name });
}

// Open the row's action dropdown and click a menu item directly.
//
// History: this helper used keyboard-relative navigation because the row menus
// rendered at floating-ui's off-screen "unpositioned" placeholder (the React 18
// + non-forwardRef <Button> anchor bug, fixed in ui/button.tsx), which made
// positional clicks fail the viewport actionability check. With the anchor
// fixed the menu positions beside its trigger and a direct role-based click is
// both robust and order-independent.
async function openRowMenu(page: Page, name: string) {
  const row = policyRow(page, name);
  await expect(row).toBeVisible();
  // The row's actions cell holds a single icon-only "More" trigger button.
  await row.getByRole("button").last().click();
  // Wait for the menu to be mounted before driving it.
  await expect(page.getByRole("menuitem", { name: "Delete" })).toBeVisible();
}

async function activateRowMenuItem(page: Page, name: string, item: "Edit" | "Delete") {
  await openRowMenu(page, name);
  await page.getByRole("menuitem", { name: item }).click();
}

// Delete a policy by name via its row dropdown + the confirm AlertDialog.
async function deletePolicyViaUi(page: Page, name: string) {
  await activateRowMenuItem(page, name, "Delete");
  const confirm = page.getByRole("alertdialog");
  await expect(confirm).toBeVisible();
  await expect(confirm).toContainText(`Delete policy “${name}”?`);
  await confirm.getByRole("button", { name: "Delete policy" }).click();
  await expect(confirm).toBeHidden();
  await expect(policyRow(page, name)).toHaveCount(0);
}

// Unique-ish name so concurrent re-runs never collide on a stale row.
function uniqueName(prefix: string) {
  return `${prefix}-${Date.now().toString(36)}-${Math.floor(Math.random() * 1e4)}`;
}

// ---- specs ---------------------------------------------------------------

test.beforeEach(async ({ page }) => {
  await gotoConsole(page);
  await navTo(page, "Policies");
  // Screen header proves we navigated.
  await expect(page.getByRole("heading", { name: "Policies", exact: true })).toBeVisible();
});

test("renders the Policies screen header, description and primary action", async ({ page }) => {
  await expect(
    page.getByText(
      "Policies set a run's barrier, egress allowlist, credential grants, and lifecycle — referenced by ID (or supplied inline) when a run is created.",
    ),
  ).toBeVisible();
  // The search box and the create action are always present.
  await expect(page.getByPlaceholder("Search policies by name or id…")).toBeVisible();
  await expect(page.getByRole("button", { name: "New policy" }).first()).toBeVisible();
  await expect(page.getByRole("button", { name: "Refresh" })).toBeVisible();
});

test("shows the empty state when no policies are defined", async ({ page }) => {
  // Fresh seed has zero persisted policies (the demo policy is the in-memory
  // default, not a row), so the empty state renders with its create CTA.
  await expect(page.getByRole("heading", { name: "No policies yet" })).toBeVisible();
  await expect(
    page.getByText(
      "A policy overrides the default for specific runs — tighter or looser, referenced by its ID when you create a run.",
    ),
  ).toBeVisible();
  // The empty-state action button (second "New policy") opens the editor.
  await page.getByRole("button", { name: "New policy" }).last().click();
  await expect(editorDialog(page)).toBeVisible();
});

test("create form requires a name (client-side validation)", async ({ page }) => {
  await openCreate(page);
  const dialog = editorDialog(page);
  // The starter spec is prefilled, so the only missing field is the name. The
  // submit button is disabled while the name is empty.
  const submit = dialog.getByRole("button", { name: "Create policy" });
  await expect(submit).toBeDisabled();
  // Type then clear to confirm the disabled state tracks the name field.
  await dialog.getByLabel("Name").fill("temp");
  await expect(submit).toBeEnabled();
  await dialog.getByLabel("Name").fill("");
  await expect(submit).toBeDisabled();
  // Closing leaves the table untouched.
  await dialog.getByRole("button", { name: "Cancel" }).click();
  await expect(editorDialog(page)).toBeHidden();
});

test("create form rejects malformed JSON spec client-side", async ({ page }) => {
  const name = uniqueName("badjson");
  await openCreate(page);
  await fillEditor(page, name, "{ this is not json }");
  const dialog = editorDialog(page);
  await dialog.getByRole("button", { name: "Create policy" }).click();
  // Client-side JSON.parse failure is surfaced in the inline error region; the
  // dialog stays open and nothing is created.
  await expect(dialog.getByText(/Spec is not valid JSON/i)).toBeVisible();
  await expect(dialog).toBeVisible();
  await dialog.getByRole("button", { name: "Cancel" }).click();
  await expect(policyRow(page, name)).toHaveCount(0);
});

test("create form surfaces the server-side spec validation error (HTTP 400)", async ({ page }) => {
  const name = uniqueName("invalidspec");
  // Syntactically valid JSON but an unknown confinement class -> server 400.
  const badSpec = JSON.stringify(
    { allowed_domains: [], first_use_approval: true, min_confinement_class: "CC9" },
    null,
    2,
  );
  await openCreate(page);
  await fillEditor(page, name, badSpec);
  const dialog = editorDialog(page);
  await dialog.getByRole("button", { name: "Create policy" }).click();
  // The server's "invalid policy spec: unknown min_confinement_class" body is
  // surfaced verbatim in the editor; the dialog stays open and nothing persists.
  await expect(dialog.getByText(/invalid policy spec/i)).toBeVisible();
  await expect(dialog).toBeVisible();
  await dialog.getByRole("button", { name: "Cancel" }).click();
  await expect(editorDialog(page)).toBeHidden();
  // Empty state remains (no row created).
  await expect(policyRow(page, name)).toHaveCount(0);
});

test("round-trip: create a policy, it appears in the list, then delete it", async ({ page }) => {
  const name = uniqueName("roundtrip");
  await createPolicyViaUi(page, name);

  // The new row shows the name, the barrier chip (the CC2 wire code renders as
  // its user label "Wall", never the raw "CC2"), the grant-count chip, and the
  // egress summary. The barrier chip's tooltip carries "internal class CC2" but
  // that is a title attribute, not visible row text — so "CC2" must NOT leak.
  const row = policyRow(page, name);
  await expect(row).toContainText(name);
  await expect(row).toContainText("Wall");
  await expect(row).not.toContainText("CC2");
  await expect(row).toContainText("1 grant");
  // The egress cell renders a compact count summary (2 allowed + 1 denied),
  // not the raw domain names.
  await expect(row).toContainText("2 domains allowed");

  // Header count reflects the new policy. Serial mode + per-spec cleanup means
  // the table starts empty, so exactly one policy exists here. Assert the exact
  // singular text ("1 policy") rather than a loose /\d+ polic(y|ies)/ which would
  // also pass on the empty "0 policies" state and prove nothing.
  await expect(page.getByText("1 policy", { exact: true })).toBeVisible();

  // Clean up so the lane returns to the empty state.
  await deletePolicyViaUi(page, name);
  await expect(page.getByRole("heading", { name: "No policies yet" })).toBeVisible();
});

test("viewing a policy shows its fields: id, min confinement, egress and eligible grants", async ({ page }) => {
  const name = uniqueName("viewspec");
  await createPolicyViaUi(page, name);

  // Click the row to open the detail Sheet.
  await policyRow(page, name).click();
  // The detail sheet is a role=dialog titled by the policy name.
  const sheet = page.getByRole("dialog").filter({ hasText: name });
  await expect(sheet).toBeVisible();

  // Field labels render in the detail grid. The barrier field is labelled
  // "Barrier" and its chip shows the user label "Wall" — the CC2 wire code lives
  // only in the chip tooltip + the raw-JSON escape hatch, never as visible copy.
  await expect(sheet.getByText("Policy ID")).toBeVisible();
  await expect(sheet.getByText("Barrier")).toBeVisible();
  await expect(sheet.getByText("Created")).toBeVisible();
  await expect(sheet.getByText("Updated")).toBeVisible();
  await expect(sheet.getByText("Wall").first()).toBeVisible();

  // The raw-JSON escape hatch (collapsed by default) carries the verbatim spec —
  // expand it, then assert it holds the egress allowlist, deny list, the eligible
  // grant, and the min_confinement_class wire field (CC codes are allowed here).
  await sheet.getByText("View raw JSON").click();
  await expect(sheet).toContainText("allowed_domains");
  await expect(sheet).toContainText("api.anthropic.com");
  await expect(sheet).toContainText("denied_domains");
  await expect(sheet).toContainText("eligible_grants");
  await expect(sheet).toContainText("github_token");
  await expect(sheet).toContainText("min_confinement_class");

  // The sheet offers an Edit affordance.
  await expect(sheet.getByRole("button", { name: "Edit policy" })).toBeVisible();

  // Close the sheet (Escape) and clean up.
  await page.keyboard.press("Escape");
  await expect(sheet).toBeHidden();
  await deletePolicyViaUi(page, name);
});

test("edit round-trip: open editor from the row menu, change the spec, see it reflected", async ({ page }) => {
  const name = uniqueName("editme");
  await createPolicyViaUi(page, name);

  // Open the edit editor via the row's dropdown (keyboard-driven, see helper).
  await activateRowMenuItem(page, name, "Edit");
  const dialog = editEditorDialog(page);
  await expect(dialog).toBeVisible();
  // The editor prefills the existing name and spec.
  await expect(dialog.getByLabel("Name")).toHaveValue(name);
  await expect(dialog.getByLabel("Spec (JSON)")).toHaveValue(/min_confinement_class/);

  // Bump the confinement floor to CC3 and save.
  const editedSpec = JSON.stringify(
    {
      allowed_domains: ["api.anthropic.com"],
      first_use_approval: true,
      min_confinement_class: "CC3",
      eligible_grants: [],
    },
    null,
    2,
  );
  await dialog.getByLabel("Spec (JSON)").fill(editedSpec);
  await dialog.getByRole("button", { name: "Save changes" }).click();
  await expect(dialog).toBeHidden();

  // The row now shows the Vault barrier chip (CC3's user label) — never "CC3".
  await expect(policyRow(page, name)).toContainText("Vault");
  await expect(policyRow(page, name)).not.toContainText("CC3");

  await deletePolicyViaUi(page, name);
});

test("search filters the table and renders the 'no matching' empty state", async ({ page }) => {
  const name = uniqueName("searchable");
  await createPolicyViaUi(page, name);

  const search = page.getByPlaceholder("Search policies by name or id…");
  // Matching query keeps the row.
  await search.fill(name);
  await expect(policyRow(page, name)).toBeVisible();

  // A non-matching query yields the search-specific empty state.
  await search.fill("zzz-no-such-policy-zzz");
  await expect(page.getByRole("heading", { name: "No matching policies" })).toBeVisible();
  await expect(page.getByText("Try a different search term.")).toBeVisible();

  // Clearing the search restores the row.
  await search.fill("");
  await expect(policyRow(page, name)).toBeVisible();

  await deletePolicyViaUi(page, name);
});
