/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, ADMIN_TOKEN, gotoConsole, mockMemberRole, mockSecurityAdminRole, navTo, navToRoute } from "./fixtures";
import { DRIVE_MEMBER } from "../src/app/lib/user-drives-copy";
import { GIT_PAT_SCOPE, OPERATOR_ONLY_REASON } from "../src/app/components/wardyn/copy";
import { VIEW_REFUSAL } from "../src/app/components/wardyn/copy/console-view";
import type { RunPolicySpec } from "../src/app/lib/types";
import type { Page } from "@playwright/test";
import { readSpec, SPEC_LABEL } from "./policy-source";

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
  await dialog.getByLabel("Name", { exact: true }).fill(name);
  await dialog.getByLabel(SPEC_LABEL).fill(specJson);
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
  // ui-secretsPolicies-1: data rows are a plain <TableRow tabIndex={0}
  // onClick=.../> with NO role="button" override (policies.tsx) — a <tr>
  // inside a real <table> keeps its implicit "row" role instead, which
  // getByRole("row", …)/screen-reader table navigation depends on. Match the
  // row carrying the name, scoped to the table.
  return page.getByRole("table").getByRole("row").filter({ hasText: name });
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
  await gotoConsole(page, "admin");
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
  // The create action is always present. The search/Refresh toolbar is gated
  // to status==="ready" && policies.length>0 (ui-secretsPolicies-5, same
  // pattern as secrets.tsx/workspaces.tsx) — absent here on the fresh,
  // zero-policy seed; see the round-trip spec below for its populated-state
  // counterpart.
  await expect(page.getByRole("button", { name: "New policy" }).first()).toBeVisible();
  await expect(page.getByPlaceholder("Search policies by name or id…")).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Refresh" })).toHaveCount(0);
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
  await dialog.getByLabel("Name", { exact: true }).fill("temp");
  await expect(submit).toBeEnabled();
  await dialog.getByLabel("Name", { exact: true }).fill("");
  await expect(submit).toBeDisabled();
  // Closing leaves the table untouched.
  await dialog.getByRole("button", { name: "Cancel" }).click();
  await expect(editorDialog(page)).toBeHidden();
});

test("create form rejects a malformed spec client-side", async ({ page }) => {
  const name = uniqueName("badjson");
  await openCreate(page);
  // Not a policy in YAML or JSON (#1921: the editor opens in YAML, and JSON text is YAML too).
  await fillEditor(page, name, "allowed_domains: [");
  const dialog = editorDialog(page);
  // The parser's failure and where it is sit beside the field; the source is
  // kept as typed and Create stays held, so nothing is sent.
  await expect(dialog.getByText(/^Invalid YAML — /)).toBeVisible();
  await expect(dialog.getByText(/^Line \d+, column \d+$/)).toBeVisible();
  await expect(dialog.getByLabel(SPEC_LABEL)).toHaveAttribute("aria-invalid", "true");
  await expect(dialog.getByLabel(SPEC_LABEL)).toHaveValue("allowed_domains: [");
  await expect(dialog.getByRole("button", { name: "Create policy" })).toBeDisabled();
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

  // Header count reflects the new policy as "<filtered> of <total>" (unfiltered
  // here, so both are 1 — policies.test.tsx's "shows the toolbar with an 'X of
  // Y policies' total once ready"). Serial mode + per-spec cleanup means the
  // table starts empty, so exactly one policy exists here. Assert the exact
  // text rather than a loose /\d+ polic(y|ies)/ which would also pass on the
  // empty "0 of 0 policies" state and prove nothing.
  await expect(page.getByText("1 of 1 policy", { exact: true })).toBeVisible();

  // The search/Refresh toolbar only renders once the table is populated
  // (ui-secretsPolicies-5's status==="ready" && policies.length>0 gate) —
  // the populated-state counterpart to the header spec's empty-state check.
  await expect(page.getByPlaceholder("Search policies by name or id…")).toBeVisible();
  await expect(page.getByRole("button", { name: "Refresh" })).toBeVisible();

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
  // The collapsed policy document below also has a "Barrier" section; the grid's label comes first.
  await expect(sheet.getByText("Barrier", { exact: true }).first()).toBeVisible();
  await expect(sheet.getByText("Created")).toBeVisible();
  await expect(sheet.getByText("Updated")).toBeVisible();
  await expect(sheet.getByText("Wall").first()).toBeVisible();

  // The raw-JSON escape hatch (collapsed by default) carries the verbatim spec —
  // expand it, then assert it holds the egress allowlist, deny list, the eligible
  // grant, and the min_confinement_class wire field (CC codes are allowed here).
  await sheet.getByText("View raw JSON").click();
  // It opens on the Summary, in the console's own words; the raw keys are the YAML view's.
  await expect(sheet.getByRole("button", { name: "Summary", exact: true })).toHaveAttribute("aria-pressed", "true");
  await expect(sheet.getByText("Allowed hosts", { exact: true })).toBeVisible();
  await expect(sheet).not.toContainText("allowed_domains");
  await sheet.getByRole("button", { name: "YAML", exact: true }).click();
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
  await expect(dialog.getByLabel("Name", { exact: true })).toHaveValue(name);
  await expect(dialog.getByLabel(SPEC_LABEL)).toHaveValue(/min_confinement_class/);

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
  await dialog.getByLabel(SPEC_LABEL).fill(editedSpec);
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

// Phase 4b: PolicyEditor's body is now the shared PolicyPanel
// (wardyn/policy-panel.tsx) — template chips + a live-derived textarea instead
// of a bare Field. These two specs cover what's new: picking a template then
// editing it, and the strict-decode error path the panel's textarea now feeds.

test("picking a template chip fills the spec, and an edit to it is reflected on save", async ({ page }) => {
  const name = uniqueName("template");
  await openCreate(page);
  const dialog = editorDialog(page);
  await dialog.getByLabel("Name", { exact: true }).fill(name);

  // "CI baseline" replaces the textarea body wholesale with its template spec.
  await dialog.getByRole("button", { name: "CI baseline" }).click();
  const specBox = dialog.getByLabel(SPEC_LABEL);
  await expect(specBox).toHaveValue(/min_confinement_class: CC1/);

  // Edit a field in the filled-in spec: bump the floor from CC1 to CC2.
  const filled = await specBox.inputValue();
  await specBox.fill(filled.replace("min_confinement_class: CC1", "min_confinement_class: CC2"));
  await dialog.getByRole("button", { name: "Create policy" }).click();
  await expect(dialog).toBeHidden();

  // The row reflects the EDITED value (CC2 -> "Wall"), not the template's CC1.
  await expect(policyRow(page, name)).toContainText("Wall");

  await deletePolicyViaUi(page, name);
});

test("the safety meter moves toward Weakest when allow_all_egress is flipped on", async ({ page }) => {
  // The editor's PolicyPanel carries a live SafetyMeter graded by POST
  // /policies/grade (composer.Grade of the DOCUMENT, debounced). A safe spec
  // reads "Safest"; flipping egress to allow-all (plus the omitted idle cap =
  // never-reap) gives it two HIGH items, which is exactly the "Weakest" corner.
  await openCreate(page);
  const dialog = editorDialog(page);
  const meter = dialog.getByTestId("safety-meter");

  const safe = JSON.stringify(
    {
      allowed_domains: ["api.anthropic.com"],
      first_use_approval: "deny_with_review",
      min_confinement_class: "CC3",
      auto_stop_after_sec: 3600,
    },
    null,
    2,
  );
  await dialog.getByLabel(SPEC_LABEL).fill(safe);
  // Grade is async + debounced; toHaveAttribute auto-retries until it lands.
  await expect(meter).toHaveAttribute("data-safety", "Safest");

  const weak = JSON.stringify(
    {
      allowed_domains: [],
      allow_all_egress: true,
      first_use_approval: "always_deny",
      min_confinement_class: "CC2",
    },
    null,
    2,
  );
  await dialog.getByLabel(SPEC_LABEL).fill(weak);
  await expect(meter).toHaveAttribute("data-safety", "Weakest");

  // Read-only interaction — cancel so the table stays at its clean empty state.
  await dialog.getByRole("button", { name: "Cancel" }).click();
  await expect(editorDialog(page)).toBeHidden();
});

// #57: the push_rules structured editor's main path — add a row, an invalid
// pattern shows its live error inline (before Save ever sees it), fixing it
// clears the error, and the policy still saves. PushRulesSection's own
// component tests (policy-push-rules.test.tsx) cover every validation branch
// and the ssh_key/non-GitHub warnings in isolation; this is the one real
// browser round-trip through the actual dialog.
test("push_rules editor: add a row, an invalid pattern shows its error, fixing it saves clean", async ({ page }) => {
  const name = uniqueName("pushrules");
  await openCreate(page);
  const dialog = editorDialog(page);
  await dialog.getByLabel("Name", { exact: true }).fill(name);

  // Two "Add path" buttons exist (Deny, then Hold for review) — the first is Deny's.
  await dialog.getByRole("button", { name: "Add path" }).first().click();
  // exact: true — "Remove Deny path 1" otherwise substring-matches too.
  const row = dialog.getByLabel("Deny path 1", { exact: true });

  // A control character: no push path can contain one, and it is the row-level
  // check named in the packet's own Strings table (PUSH_RULES.ERR_CONTROL).
  await row.fill(".github/workflows/\u0007hook");
  await expect(dialog.getByRole("alert")).toHaveText(
    "This pattern has a control character in it, which no push path can contain.",
  );

  // Fixing the pattern clears the row error — advisory only, never blocking typing.
  await row.fill(".github/workflows/**");
  await expect(dialog.getByRole("alert")).toHaveCount(0);

  // The row-level checks above only prove the EDITOR's own opinion — assert
  // what actually reaches the server. Capture the real POST body rather than
  // trusting the dialog closing/the row appearing.
  const created = page.waitForRequest(
    (r) => r.url().includes("/api/v1/policies") && r.method() === "POST",
  );
  await dialog.getByRole("button", { name: "Create policy" }).click();
  const body = (await created).postDataJSON() as { spec: RunPolicySpec };
  expect(body.spec.push_rules).toEqual({ deny_paths: [".github/workflows/**"] });

  await expect(dialog).toBeHidden();
  await expect(policyRow(page, name)).toBeVisible();

  // Round-trip: what the server actually stored (not just what was posted)
  // carries the same push_rules — opens the just-created policy's raw-JSON
  // escape hatch and reads it back.
  await policyRow(page, name).click();
  const sheet = page.getByRole("dialog").filter({ hasText: name });
  await sheet.getByText("View raw JSON").click();
  // The Summary names the rule in words; the YAML view carries the raw keys.
  await expect(sheet.getByText("Deny", { exact: true })).toBeVisible();
  await sheet.getByRole("button", { name: "YAML", exact: true }).click();
  await expect(sheet).toContainText("push_rules");
  await expect(sheet).toContainText("deny_paths");
  await expect(sheet).toContainText(".github/workflows/**");
  await page.keyboard.press("Escape");
  await expect(sheet).toBeHidden();

  await deletePolicyViaUi(page, name);
});

test("create form surfaces the server-side error for an unknown spec key (strict decode)", async ({ page }) => {
  const name = uniqueName("unknownkey");
  // Syntactically valid JSON, structurally valid otherwise, but a key the
  // server's strict decoder (decodeStrictMsg, DisallowUnknownFields) has never
  // heard of -> rejected before validatePolicySpec even runs. A different code
  // path than the CC9 enum-rejection spec above.
  const badSpec = JSON.stringify(
    {
      allowed_domains: [],
      first_use_approval: true,
      min_confinement_class: "CC2",
      not_a_real_field: true,
    },
    null,
    2,
  );
  await openCreate(page);
  await fillEditor(page, name, badSpec);
  const dialog = editorDialog(page);
  await dialog.getByRole("button", { name: "Create policy" }).click();
  // The server's "invalid JSON body: json: unknown field ..." message is
  // surfaced verbatim; the dialog stays open and nothing persists.
  await expect(dialog.getByText(/unknown field/i)).toBeVisible();
  await expect(dialog).toBeVisible();
  await dialog.getByRole("button", { name: "Cancel" }).click();
  await expect(editorDialog(page)).toBeHidden();
  await expect(policyRow(page, name)).toHaveCount(0);
});

// The reserved user-drive target (0.7). A policy may not name /home/agent/drive
// as a mount target: the member's own drive mounts there, and the two are
// authored by different people at different times — an admin writes the policy,
// an admin allocates the drive, a member ticks a checkbox at run time — so a
// collision would surface as one of them silently disappearing inside a running
// sandbox rather than as a refusal anybody could act on.
//
// The expected text IS the frozen canon (DRIVE_MEMBER.REFUSED_TARGET_RESERVED,
// user-drives-prompt.md §7.7) — asserted as a string, byte for byte, with no
// regex softening in between. It can be, because the canon spells exactly what
// the wire carries: runner.ValidateAuthoredTarget composes the sentence and the
// caller prefixes the field index its own convention already adds, so the canon
// entry spells `workspace_mounts[0]` too, and the path is PLAIN — a mono span
// is a display concern the console applies, never bytes baked into the string
// (internal/runner/mount.go:76, ui/src/app/lib/user-drives-copy.ts's backtick
// rule). A message that drifts from canon in either direction fails here.
//
// Substring, because the handler prefixes its own "invalid policy spec: " —
// that prefix is the API's, shared by every spec refusal, and is pinned by the
// sibling cases above rather than folded into this feature's canon.

test("create form surfaces the reserved user-drive target refusal (HTTP 400)", async ({ page }) => {
  const name = uniqueName("reservedtarget");
  // A structurally valid mount whose SOURCE passes the bind-mount deny-list —
  // so the only thing wrong with it is the target, and the refusal names it.
  const reservedSpec = JSON.stringify(
    {
      allowed_domains: [],
      first_use_approval: true,
      min_confinement_class: "CC2",
      workspace_mounts: [{ source: "/home/me/projects/payments", target: "/home/agent/drive" }],
    },
    null,
    2,
  );
  await openCreate(page);
  await fillEditor(page, name, reservedSpec);
  const dialog = editorDialog(page);
  await dialog.getByRole("button", { name: "Create policy" }).click();

  // validatePolicySpec routes every AUTHORED target through
  // runner.ValidateAuthoredTarget, and its message is surfaced verbatim.
  await expect(dialog.getByText(DRIVE_MEMBER.REFUSED_TARGET_RESERVED)).toBeVisible();
  await expect(dialog).toBeVisible();
  await dialog.getByRole("button", { name: "Cancel" }).click();
  await expect(editorDialog(page)).toBeHidden();
  // Nothing persisted: the policy table is untouched.
  await expect(policyRow(page, name)).toHaveCount(0);
});

// X2-F12 — GET /policies is readable by any signed-in human (routes.go: "a
// signed-in MEMBER can read policies but not CRUD them"); only the writes
// (POST/PUT/DELETE) are operatorOnly. This file only ever walked it as an
// admin, so nothing pinned the read-only render for the two other tiers.
// security_admin is the interesting one: /admin/policies gates on plain
// useOperator(), the SAME OPERATOR_ONLY_REASON a member would get — unlike
// /admin/permissions (SECURITY_ONLY_REASON, permissions.spec.ts), the
// security tier earns this screen NO extra reach. Read-only: neither test
// creates a row, so the file's empty-table invariant (header comment) holds
// either way. M-1b: Policies moved to /admin/policies — a MEMBER never
// reaches the screen's own reason any more (this harness's security-admin
// splice has no real SSO session, so its access stays the permissive "url"
// tier and it still passes the view gate straight through; only a member's
// role check is unconditional, admin-member-modes-design.md §2.1).
test.describe("Policies — member and security-admin reads", () => {
  // ticket: X2-F12
  test("a member is refused the admin view before the list ever loads", async ({ page }) => {
    await mockMemberRole(page);
    await gotoConsole(page);
    await navToRoute(page, "/admin/policies");
    await expect(page.getByRole("heading", { name: VIEW_REFUSAL.TITLE })).toBeVisible();
    await expect(page.getByText(VIEW_REFUSAL.BODY)).toBeVisible();
    await expect(page.getByRole("heading", { name: "Policies", level: 1 })).toHaveCount(0);
  });

  test("a security admin reads the list too, and gets the SAME parked writes — security is not operator here", async ({
    page,
  }) => {
    await mockSecurityAdminRole(page);
    await gotoConsole(page, "admin");
    await navTo(page, "Policies");
    await expect(page.getByRole("heading", { name: "Policies", level: 1 })).toBeVisible();
    await expect(page.getByText(OPERATOR_ONLY_REASON).first()).toBeVisible();
    await expect(page.getByRole("button", { name: "New policy" })).toBeDisabled();
  });
});

// Stored-token narrowing (gpat-g3, packet M7): repos, access, api and forge on a
// stored-token grant, authored in the editor's "Git access tokens" section and
// read back from what the server stored. Each test creates and deletes its own
// policy, so the file's empty-table invariant holds.
test.describe("Policies — stored token narrowing", () => {
  const PAT = { host: "gitlab.example.com", secret_name: "git-pat-gitlab-example-com" };
  const PAT_SPEC = JSON.stringify(
    {
      allowed_domains: [],
      first_use_approval: "always_deny",
      min_confinement_class: "CC2",
      eligible_grants: [{ kind: "git_pat", scope: PAT, ttl_seconds: 3600, requires_approval: false }],
    },
    null,
    2,
  );

  test("saves forge, repositories, access and API, and reads all four back", async ({ page, request }) => {
    const name = uniqueName("e2e-pat-narrow");
    await gotoConsole(page, "admin");
    await navTo(page, "Policies");
    await openCreate(page);
    const dialog = editorDialog(page);
    await fillEditor(page, name, PAT_SPEC);

    // The honesty lines are on screen before anything is narrowed.
    const honesty = dialog.getByTestId("git-pat-honesty");
    await expect(honesty.getByText(GIT_PAT_SCOPE.HONESTY_TOKEN)).toBeVisible();
    await expect(honesty.getByText(GIT_PAT_SCOPE.HONESTY_API)).toBeVisible();
    await expect(honesty.getByText(GIT_PAT_SCOPE.HONESTY_BROKER)).toBeVisible();

    // The generic forge has no API door: the box is off and says why.
    await expect(dialog.getByLabel(GIT_PAT_SCOPE.FORGE, { exact: true })).toHaveValue("generic");
    await expect(dialog.getByRole("checkbox", { name: GIT_PAT_SCOPE.API })).toBeDisabled();
    await expect(dialog.getByText(GIT_PAT_SCOPE.API_NEEDS_FORGE)).toBeVisible();

    await dialog.getByLabel(GIT_PAT_SCOPE.FORGE, { exact: true }).selectOption("gitlab");
    await dialog.getByLabel(GIT_PAT_SCOPE.REPOS, { exact: true }).fill("group/app\ngroup/libs/*");
    await dialog.getByRole("button", { name: "Read-only", exact: true }).click();
    await dialog.getByRole("checkbox", { name: GIT_PAT_SCOPE.API }).click();

    const spec = await readSpec<RunPolicySpec>(dialog.getByLabel(SPEC_LABEL));
    expect(spec.eligible_grants?.[0].scope).toEqual({
      ...PAT,
      forge: "gitlab",
      repos: ["group/app", "group/libs/*"],
      access: "read",
      api: true,
    });

    const created = page.waitForResponse((r) => r.url().includes("/api/v1/policies") && r.request().method() === "POST");
    await dialog.getByRole("button", { name: "Create policy" }).click();
    const res = await created;
    expect(res.status()).toBe(201);
    const stored = (await res.json()) as { id: string; spec: RunPolicySpec };
    try {
      expect(stored.spec.eligible_grants?.[0].scope).toMatchObject({
        forge: "gitlab",
        repos: ["group/app", "group/libs/*"],
        access: "read",
        api: true,
      });
      await expect(dialog).toBeHidden();

      // Reopen from the row: the fields show what the server stored.
      await policyRow(page, name).click();
      await page.getByRole("dialog").filter({ hasText: name }).getByRole("button", { name: "Edit policy" }).click();
      const edit = editEditorDialog(page);
      await expect(edit).toBeVisible();
      await expect(edit.getByLabel(GIT_PAT_SCOPE.FORGE, { exact: true })).toHaveValue("gitlab");
      await expect(edit.getByLabel(GIT_PAT_SCOPE.REPOS, { exact: true })).toHaveValue("group/app\ngroup/libs/*");
      await expect(edit.getByRole("button", { name: "Read-only", exact: true })).toHaveAttribute("aria-pressed", "true");
      await expect(edit.getByRole("checkbox", { name: GIT_PAT_SCOPE.API })).toBeChecked();
      await edit.getByRole("button", { name: "Cancel" }).click();
      await expect(edit).toBeHidden();
    } finally {
      const del = await request.delete(`/api/v1/policies/${stored.id}`, { headers: { Authorization: `Bearer ${ADMIN_TOKEN}` } });
      expect(del.ok()).toBe(true);
    }
  });

  test("a refusal that names an axis lands on that field", async ({ page }) => {
    const name = uniqueName("e2e-pat-axis");
    await gotoConsole(page, "admin");
    await navTo(page, "Policies");
    await openCreate(page);
    const dialog = editorDialog(page);
    await fillEditor(page, name, PAT_SPEC);

    const repos = dialog.getByLabel(GIT_PAT_SCOPE.REPOS, { exact: true });
    await repos.fill("group/../elsewhere");
    await dialog.getByRole("button", { name: "Create policy" }).click();

    // The server's own sentence, under Repositories, with the field marked invalid.
    await expect(repos).toHaveAttribute("aria-invalid", "true");
    await expect(dialog.getByRole("alert").filter({ hasText: "repos entry" }).first()).toBeVisible();
    await expect(dialog.getByLabel(GIT_PAT_SCOPE.FORGE, { exact: true })).not.toHaveAttribute("aria-invalid", "true");
    await dialog.getByRole("button", { name: "Cancel" }).click();
    await expect(editorDialog(page)).toBeHidden();
    await expect(policyRow(page, name)).toHaveCount(0);
  });
});
