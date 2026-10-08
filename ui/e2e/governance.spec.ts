/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { randomUUID } from "node:crypto";
import {
  test,
  expect,
  ADMIN_TOKEN,
  gotoConsole,
  asRealSecurityAdmin,
  consoleAPI,
  navTo,
  navToRoute,
  sidebarLink,
  sql,
} from "./fixtures";
import { GOVERNANCE as GOV, LIMITS_CHIP, PEOPLE, PERM, PREVIEW, RUBRIC } from "../src/app/lib/governance-copy";
import { AUTONOMY_META } from "../src/app/components/wardyn/autonomy-meta";
import { OPERATOR_ONLY_REASON } from "../src/app/components/wardyn/copy";
import type { Page } from "@playwright/test";
import { SPEC_LABEL } from "./policy-source";

// ---------------------------------------------------------------------------
// Governance profiles e2e (0.7) — lane: governance, port 8288, db wardyn_e2e.
//
// #209 split this file by behaviour: the member's own view of a ceiling
// (route-spliced, independent of this file's real backend state) lives in
// governance-member.spec.ts, and the self-contained profile-limits round
// trips (quota, storage ceilings, run limits — each creates its own named
// profile) live in governance-profile-limits.spec.ts. What stays HERE is the
// tightly-coupled core this file's own serial ordering exists for: the
// authoring walk builds a real profile ("walled") and a real group
// assignment that the resolved-preview, security-admin and wall-refusal
// suites below all read back from the SAME Postgres row — none of that can
// be split across files, each of which gets its OWN freshly seeded backend
// (scripts/run-ui-e2e.sh).
//
// Every expected string is IMPORTED from lib/governance-copy.ts (which itself
// re-exports the §7.1 canon it reuses — PERM/PEOPLE/PREVIEW — so this file has
// ONE import site for product copy). Nothing here retypes a sentence: a copy
// change must break this spec rather than let the screen drift away from
// docs/design/governance-prompt.md §7 and governance-mock/index.html.
//
// WHAT IS BROWSER-LEVEL AND WHAT IS API-LEVEL, and why
// ----------------------------------------------------
// The authoring surface is driven for real: the seven securityOps routes
// behind /governance are all reachable with the seeded admin bearer, so the
// walk below writes to Postgres — profile created, group assigned, resolved
// preview answered by the SERVER's own resolver, delete refused by ON DELETE
// RESTRICT.
//
// What CANNOT be browser-level here, and is asserted through the API or
// spliced instead, with the reason in each place:
//
//   1. A RUN BOUND BY A PROFILE. effectiveCeiling (internal/api/governance.go)
//      short-circuits at step 1: "OPERATOR ⇒ DefaultPolicy, with NO store
//      read". The operator token used by this authoring walk takes that arm,
//      so its GET /policies/default omits governance_profile_name. The none
//      runner cannot dispatch a profile-bound sandbox; the
//      walls are asserted at the API level instead (the resolver, the stored
//      ceiling, the two write refusals), and the run-level enforcement is
//      proven in Go — TestGovernanceProfileNonEscape's 16-row escape table on
//      the decoded run.policy.resolve envelope, and
//      governance_ceiling_test.go's governance_profile_name pin.
//
//   2. THE MEMBER'S TWO DISPLAY MOMENTS. Same cause: the field the rail and
//      the Getting Started card read is the one the operator short-circuit
//      omits — see governance-member.spec.ts for that splice.
//
//   3. The security-admin block below uses a real restricted token and
//      server refusals.
//
// Serial: one backend, one profiles table. The walk builds state the later
// blocks read, and a mutating test must never run beside an assertion about
// the same rows.
test.describe.configure({ mode: "serial" });

const NAME = "walled";
const GROUP = "eng-contractors";
const USER = "contractor@corp.example";

// The §E north-star ceiling: an agent that may run autonomously (tool_rules
// all-allow — no call parks for a human) but is hard-walled anyway. Every
// company host denied, the barrier floored above the deployment's CC1, and NO
// eligible grants at all — which is what makes credential injection to those
// hosts structurally impossible rather than merely refused: there is no grant
// kind under this ceiling for the proxy to mint.
const YOLO_CEILING = {
  allowed_domains: ["api.anthropic.com"],
  denied_domains: ["github.example.com", "artifactory.corp.example.com"],
  first_use_approval: "deny_with_review",
  min_confinement_class: "CC3",
  auto_stop_after_sec: 3600,
  eligible_grants: [],
  tool_rules: [{ tool: "*", effect: "allow" }],
};

// A ceiling that tries to MINT credential eligibility the deployment does not
// grant (contents:write over the default's contents:read, a doubled TTL, and
// requires_approval stripped). governanceGrantWithinCeiling's monotone-⊆ bound
// refuses it — this is the "the developer cannot loosen it" leg, authored from
// the same editor an admin would use.
const OVERWIDE_CEILING = {
  ...YOLO_CEILING,
  eligible_grants: [
    {
      kind: "github_token",
      scope: { repos: [], permissions: { contents: "write" } },
      ttl_seconds: 7200,
      requires_approval: false,
    },
  ],
};

// CONSOLE-RULES §6 — exactly ONE `default`-variant (teal) button on the screen
// at any moment. The default variant is `bg-primary` (ui/button.tsx:12), and
// LimitRow's switch paints bg-primary too when checked (profile-editor.tsx:196)
// — excluded, because a toggle is not an action. Scoped to <main>: the app
// shell's own "New run" in the top bar is a default button on every screen and
// is not this surface's.
const tealActions = (page: Page) => page.locator("main").locator('button.bg-primary:not([role="switch"])');

// The screen draws TWO tables, and a profile NAME is a cell in both — the
// profiles list's Name column and the assignments list's Profile column. Every
// row assertion says which one it means, or it matches two elements the moment
// an assignment exists (which is exactly the state most of this walk is in).
const profilesTable = (page: Page) => page.getByRole("table").first();
const assignmentsTable = (page: Page) => page.getByRole("table").nth(1);

// The claims the member's token would carry, and the answer the SERVER gives.
// The preview posts every typed line to BOTH tiers (assignments.tsx) — the
// response is what names which one matched.
async function resolve(page: Page, claims: string): Promise<void> {
  await page.locator("#governance-preview-claims").fill(claims);
  await page.getByRole("button", { name: GOV.PREVIEW_RUN_CTA, exact: true }).click();
}

// ---------------------------------------------------------------------------
// 1. The authoring walk — empty install → a profile → an assignment.
// ---------------------------------------------------------------------------

test.describe("governance — the security admin's authoring walk", () => {
  test("a fresh install: no profiles, and the empty state carries the action that fills it", async ({ page }) => {
    await gotoConsole(page, "admin");
    await navTo(page, "Governance");

    await expect(page.getByRole("heading", { name: GOV.TITLE, level: 1 })).toBeVisible();
    await expect(page.getByText(GOV.LEAD)).toBeVisible();
    await expect(page.getByText(GOV.PROFILES_LEAD)).toBeVisible();
    await expect(page.getByText(GOV.EMPTY_TITLE)).toBeVisible();
    // Rendered through withMono (WARDYN_DEFAULT_POLICY is an env var), so the
    // string is split across a <p> and a mono <span> — the <p> is still the
    // smallest element carrying all of it.
    await expect(page.getByText(GOV.EMPTY_BODY)).toBeVisible();

    // Both empty states, and the one that is not a state at all: with no
    // profile there is nothing to assign, and the screen says so rather than
    // offering an add form that cannot succeed.
    await expect(page.getByText(GOV.EMPTY_ASSIGN_TITLE)).toBeVisible();
    await expect(page.getByText(GOV.EMPTY_ASSIGN_BODY)).toBeVisible();

    // The two standing facts the screen must always show: precedence, and the
    // two-subject halves of "when does this take effect".
    await expect(page.getByText(GOV.PRECEDENCE)).toBeVisible();
    await expect(page.getByText(GOV.EFFECT_NOTE)).toBeVisible();
    await expect(page.getByText(GOV.SIGNIN_NOTE)).toBeVisible();

    // ONE teal, and on an empty install it is the empty state's own New
    // profile — never the add form's Assign, which has no profile to bind.
    await expect(tealActions(page)).toHaveCount(1);
    await expect(page.getByRole("button", { name: GOV.NEW_CTA, exact: true })).toHaveClass(/bg-primary/);
  });

  test("creating a profile: the editor opens in place, and the save warns what it takes away", async ({ page }) => {
    await gotoConsole(page, "admin");
    await navTo(page, "Governance");

    await page.getByRole("button", { name: GOV.NEW_CTA, exact: true }).click();

    // IN PLACE, under the list it edits — not a dialog.
    const editor = page.getByTestId("governance-profile-editor");
    await expect(editor).toBeVisible();
    await expect(page.getByRole("alertdialog")).toHaveCount(0);
    await expect(editor.getByRole("heading", { name: GOV.EDITOR_TITLE_NEW })).toBeVisible();
    await expect(editor.getByText(GOV.NAME_HINT)).toBeVisible();
    await expect(editor.getByText(GOV.CEILING_LEAD)).toBeVisible();
    await expect(editor.getByText(GOV.LIMITS_LEAD)).toBeVisible();
    await expect(editor.getByText(GOV.GRADE_NOTE)).toBeVisible();

    await page.locator("#governance-profile-name").fill(NAME);
    // The SHIPPED spec editor (PolicyPanel instance="policies"), reached the
    // same way policies.spec.ts reaches it — there is no second spec editor.
    // The ceiling reads first (#1921); "Edit policy" opens its source.
    await editor.getByRole("button", { name: "Edit policy", exact: true }).click();
    await expect(editor.getByLabel(SPEC_LABEL)).toBeFocused();
    await editor.getByLabel(SPEC_LABEL).fill(JSON.stringify(YOLO_CEILING, null, 2));

    // The two launch modes a ceiling cannot reach, denied here instead.
    await editor.getByRole("switch", { name: GOV.LIMIT_EXEC_LABEL }).click();
    await editor.getByRole("switch", { name: GOV.LIMIT_INTERACTIVE_LABEL }).click();
    await expect(editor.getByRole("switch", { name: GOV.LIMIT_EXEC_LABEL })).toHaveAttribute("aria-checked", "true");

    // While the editor is open, Save profile is the surface's ONE teal — the
    // add form below has collapsed and taken its Assign with it.
    await expect(tealActions(page)).toHaveCount(1);
    await expect(page.getByRole("button", { name: GOV.SAVE, exact: true })).toHaveClass(/bg-primary/);

    await page.getByRole("button", { name: GOV.SAVE, exact: true }).click();

    // Q6's adopted variant: the omission list renders AFTER a successful save,
    // never blocking it — the server's own prose under the frozen heading.
    // This ceiling carries no eligible grants at all, which is the §E wall:
    // nothing under it can request a credential.
    await expect(page.getByText(GOV.OMISSION_TITLE)).toBeVisible();
    await expect(page.getByText(/omits 1 eligible grant kind\(s\).*github_token/)).toBeVisible();

    // The row, in the list's own vocabulary.
    const row = profilesTable(page).getByRole("row", { name: new RegExp(NAME) });
    await expect(row.getByRole("cell", { name: NAME, exact: true })).toBeVisible();
    await expect(row.getByText(GOV.ASSIGNED_NONE)).toBeVisible();
    await expect(row.getByText(GOV.LIMIT_EXEC_LABEL)).toBeVisible();
    await expect(row.getByText(GOV.LIMIT_INTERACTIVE_LABEL)).toBeVisible();
    await expect(page.getByText(GOV.EMPTY_TITLE)).toHaveCount(0);

    // It was a real write, not local state.
    await page.reload();
    await expect(profilesTable(page).getByRole("cell", { name: NAME, exact: true })).toBeVisible();
  });

  test("assigning it to a group lands the row, its precedence, and the profile's new count", async ({ page }) => {
    await gotoConsole(page, "admin");
    await navTo(page, "Governance");

    // The subject Segmented defaults to Group (assignments.tsx) — assert it
    // rather than assume, since the hint below the field is derived from it.
    await expect(page.getByRole("button", { name: PERM.SUBJECT_GROUP, exact: true })).toHaveAttribute(
      "aria-pressed",
      "true",
    );
    await expect(page.getByText(PERM.HINT_GROUP)).toBeVisible();

    await page.getByRole("textbox", { name: PERM.FIELD_WHO, exact: true }).fill(GROUP);
    await page.locator("#governance-assignment-profile").click();
    await page.getByRole("option", { name: NAME, exact: true }).click();
    await page.locator("#governance-assignment-priority").fill("10");

    // At rest, with a profile to bind, Assign is the surface's one teal.
    await expect(tealActions(page)).toHaveCount(1);
    const assign = page.getByRole("button", { name: GOV.ADD_CTA, exact: true });
    await expect(assign).toHaveClass(/bg-primary/);
    await assign.click();

    const assignments = assignmentsTable(page);
    const row = assignments.getByRole("row", { name: new RegExp(GROUP) });
    await expect(row.getByText(PERM.SUBJECT_GROUP, { exact: true })).toBeVisible();
    await expect(row.getByText(GROUP, { exact: true })).toBeVisible();
    await expect(row.getByRole("cell", { name: NAME, exact: true })).toBeVisible();
    // Priority is meaningful only inside the group tier, and this row is one.
    await expect(row.getByRole("cell", { name: "10", exact: true })).toBeVisible();
    await expect(page.getByText(GOV.EMPTY_ASSIGN_TITLE)).toHaveCount(0);

    // The profiles list above re-reads the same snapshot: one subject now.
    await expect(page.getByText(GOV.ASSIGNED_COUNT(1), { exact: true })).toBeVisible();
    await expect(page.getByText(GOV.ASSIGNED_NONE)).toHaveCount(0);
  });

  test("the editor opens in place and the add form COLLAPSES while it is open", async ({ page }) => {
    await gotoConsole(page, "admin");
    await navTo(page, "Governance");

    await page.getByRole("button", { name: `${GOV.EDIT} ${NAME}`, exact: true }).click();

    const editor = page.getByTestId("governance-profile-editor");
    await expect(editor).toBeVisible();
    await expect(editor.getByRole("heading", { name: GOV.EDITOR_TITLE_EDIT(NAME) })).toBeVisible();
    // It opened on the SAVED ceiling, not a fresh starter.
    await expect(editor.getByTestId("policy-document")).toContainText("artifactory.corp.example.com");
    await expect(editor.getByLabel(SPEC_LABEL)).toHaveCount(0);
    await expect(editor.getByRole("switch", { name: GOV.LIMIT_EXEC_LABEL })).toHaveAttribute("aria-checked", "true");

    // The collapse: a disabled summary row where the add form was — the form is
    // DISABLED, not removed (§7's "a busy control is disabled, not removed").
    await expect(page.getByTestId("governance-add-assignment-collapsed")).toBeVisible();
    const assign = page.getByRole("button", { name: GOV.ADD_CTA, exact: true });
    await expect(assign).toBeDisabled();
    await expect(assign).not.toHaveClass(/bg-primary/);
    // The whole row of per-profile actions parks too, so a second editor can
    // never open over this one.
    await expect(page.getByRole("button", { name: GOV.NEW_CTA, exact: true })).toBeDisabled();
    await expect(page.getByRole("button", { name: `${GOV.EDIT} ${NAME}`, exact: true })).toBeDisabled();

    // ONE default-weight action on the surface, and it is Save profile.
    await expect(tealActions(page)).toHaveCount(1);
    await expect(page.getByRole("button", { name: GOV.SAVE, exact: true })).toHaveClass(/bg-primary/);

    await page.getByRole("button", { name: PEOPLE.CANCEL, exact: true }).click();

    // …and the teal goes back to Assign the moment the editor closes.
    await expect(page.getByTestId("governance-profile-editor")).toHaveCount(0);
    await expect(page.getByTestId("governance-add-assignment-collapsed")).toHaveCount(0);
    await expect(tealActions(page)).toHaveCount(1);
    await expect(page.getByRole("button", { name: GOV.ADD_CTA, exact: true })).toHaveClass(/bg-primary/);
  });

  // #93/#96 — the autonomy rubric round-trips through the editor: a saved row
  // survives a reload, and the profiles-list chip names the STRICTEST cap
  // (ruling 2, #96 review), not merely that a rubric exists.
  //
  // Its OWN profile, never "walled" — this walk is serial, and the later
  // "the stored ceiling IS the wall" test asserts `walled`'s `limits` with a
  // strict toEqual; adding a rubric onto that same profile would fail it for
  // a reason that test's own diff cannot explain.
  const RUBRIC_NAME = "autonomy-rubric-e2e";
  test("the autonomy rubric round-trips through the editor, and the profiles list names the strictest cap", async ({
    page,
  }) => {
    await gotoConsole(page, "admin");
    await navTo(page, "Governance");

    await page.getByRole("button", { name: GOV.NEW_CTA, exact: true }).click();
    let editor = page.getByTestId("governance-profile-editor");
    await page.locator("#governance-profile-name").fill(RUBRIC_NAME);
    await expect(editor.getByText(RUBRIC.HEADING)).toBeVisible();
    // Untouched: every row reads No cap.
    await expect(
      editor.getByRole("combobox", { name: `${RUBRIC.ROWS.secrets_powerful[0]} caps autonomy at`, exact: true }),
    ).toHaveText(RUBRIC.NOCAP);

    // Two rows, at two different levels — the lower one is what the chip and
    // the footer note must name.
    await editor.getByRole("combobox", { name: `${RUBRIC.ROWS.secrets_powerful[0]} caps autonomy at`, exact: true }).click();
    await page.getByRole("option", { name: AUTONOMY_META.L2.label, exact: true }).click();
    await editor.getByRole("combobox", { name: `${RUBRIC.ROWS.confinement_cc1[0]} caps autonomy at`, exact: true }).click();
    await page.getByRole("option", { name: AUTONOMY_META.L1.label, exact: true }).click();
    await expect(editor.getByText(RUBRIC.SET_NOTE(2, AUTONOMY_META.L1.label))).toBeVisible();

    await editor.getByRole("button", { name: GOV.SAVE, exact: true }).click();
    await expect(editor).toHaveCount(0);

    // The profiles list chip: the strictest cap, on the chip face itself.
    const row = profilesTable(page).getByRole("row", { name: new RegExp(RUBRIC_NAME) });
    await expect(row.getByText(LIMITS_CHIP.AUTONOMY(AUTONOMY_META.L1.label))).toBeVisible();

    // It was a real write, not local state — reload and reopen the editor.
    await page.reload();
    await page.getByRole("button", { name: `${GOV.EDIT} ${RUBRIC_NAME}`, exact: true }).click();
    editor = page.getByTestId("governance-profile-editor");
    await expect(
      editor.getByRole("combobox", { name: `${RUBRIC.ROWS.secrets_powerful[0]} caps autonomy at`, exact: true }),
    ).toHaveText(AUTONOMY_META.L2.label);
    await expect(
      editor.getByRole("combobox", { name: `${RUBRIC.ROWS.confinement_cc1[0]} caps autonomy at`, exact: true }),
    ).toHaveText(AUTONOMY_META.L1.label);
  });

  test("delete is REFUSED while assigned: pre-filled from the count, and the confirm never enables", async ({
    page,
  }) => {
    await gotoConsole(page, "admin");
    await navTo(page, "Governance");

    await page.getByRole("button", { name: `${GOV.DELETE} ${NAME}`, exact: true }).click();

    const dialog = page.getByRole("alertdialog");
    // The question half of DELETE_CONFIRM titles both variants; only the
    // consequence differs, and this one's is the restriction.
    await expect(dialog.getByRole("heading", { name: `Delete "${NAME}"?` })).toBeVisible();
    await expect(dialog.getByText(GOV.DELETE_RESTRICT_TITLE)).toBeVisible();
    await expect(dialog.getByText(GOV.DELETE_RESTRICT_BODY(NAME, 1))).toBeVisible();
    // The unassigned consequence would be a false claim here.
    await expect(dialog.getByText(GOV.DELETE_CONFIRM(NAME))).toHaveCount(0);

    // There is nothing to attempt: the list already knows the count, so the
    // confirm is disabled rather than left to fail (§2.4).
    await expect(dialog.getByRole("button", { name: GOV.DELETE, exact: true })).toBeDisabled();

    await dialog.getByRole("button", { name: PEOPLE.CANCEL, exact: true }).click();
    // Walked away from: the profile is still there.
    await expect(profilesTable(page).getByRole("cell", { name: NAME, exact: true })).toBeVisible();
  });
});

// ---------------------------------------------------------------------------
// 2. The resolved preview — the answer names the tier that matched.
// ---------------------------------------------------------------------------

test.describe("governance — the resolved preview", () => {
  test("pasted claims resolve to the profile, named with the assignment that won", async ({ page }) => {
    await gotoConsole(page, "admin");
    await navTo(page, "Governance");

    await expect(page.getByText(GOV.PREVIEW_LEAD)).toBeVisible();
    // The People step's own field and hint, verbatim — one accepted shape.
    await expect(page.getByText(PREVIEW.FIELD_CLAIMS, { exact: true })).toBeVisible();
    await expect(page.getByText(PREVIEW.FIELD_CLAIMS_HINT)).toBeVisible();

    await resolve(page, GROUP);
    // The tier is the point: this answer could only have come from the group
    // assignment, and it says so.
    await expect(page.getByText(GOV.PREVIEW_RESULT(NAME, GOV.MATCHED_GROUP))).toBeVisible();

    // Nothing was saved, and the screen says that too.
    await expect(page.getByText(GOV.PREVIEW_NOT_SAVED)).toBeVisible();
  });

  test("claims nothing matches resolve to the deployment ceiling, not to a profile", async ({ page }) => {
    await gotoConsole(page, "admin");
    await navTo(page, "Governance");

    await resolve(page, "no-such-group\nnobody@corp.example");
    await expect(page.getByText(GOV.PREVIEW_RESULT_DEFAULT)).toBeVisible();
    await expect(page.getByText(GOV.PREVIEW_RESULT(NAME, GOV.MATCHED_GROUP))).toHaveCount(0);
  });
});

// ---------------------------------------------------------------------------
// 3. The write refusal the server composes — rendered verbatim under the
//    console's own heading, never re-worded.
// ---------------------------------------------------------------------------

test.describe("governance — a ceiling that would MINT credential eligibility is refused", () => {
  test("the grant-bound refusal renders the server's message under the frozen heading", async ({ page }) => {
    await gotoConsole(page, "admin");
    await navTo(page, "Governance");

    await page.getByRole("button", { name: GOV.NEW_CTA, exact: true }).click();
    const editor = page.getByTestId("governance-profile-editor");
    await page.locator("#governance-profile-name").fill("wider-than-the-deployment");
    await editor.getByRole("button", { name: "Edit policy", exact: true }).click();
    await editor.getByLabel(SPEC_LABEL).fill(JSON.stringify(OVERWIDE_CEILING, null, 2));
    await page.getByRole("button", { name: GOV.SAVE, exact: true }).click();

    // The heading is the console's; the body is the SERVER's own prose, which
    // names the failing leg. A frozen sentence here would have had to drop it.
    await expect(editor.getByText(GOV.GRANT_BOUND_TITLE)).toBeVisible();
    // The prefix isGrantBoundError discriminates on (lib/api/governance.ts) —
    // a plain spec-validation 400 shares "invalid ceiling: " but never this.
    await expect(editor.getByText(/invalid ceiling: eligible grant "github_token"/)).toBeVisible();

    // Nothing was written.
    await page.getByRole("button", { name: PEOPLE.CANCEL, exact: true }).click();
    await expect(profilesTable(page).getByRole("cell", { name: "wider-than-the-deployment", exact: true })).toHaveCount(0);
  });
});

// ---------------------------------------------------------------------------
// 4. Security-admin authorization, with the negative control right after it
//    (that block reads a run row this one's last test writes).
// ---------------------------------------------------------------------------

test.describe("governance — the security admin's console (real per-person token)", () => {
  test.beforeEach(async ({ page }) => {
    await asRealSecurityAdmin(page);
  });

  test("Governance, Permissions and Audit are offered — and Governance is ACTIONABLE", async ({ page }) => {
    await gotoConsole(page, "admin");

    await expect(sidebarLink(page, "Governance")).toBeVisible();
    await expect(sidebarLink(page, "Permissions")).toBeVisible();
    await expect(sidebarLink(page, "Audit")).toBeVisible();
    await expect(sidebarLink(page, "Approvals")).toBeVisible();

    // Offered is not enough: the tier's whole purpose is authoring ceilings,
    // so the write controls must be live, not merely visible. Each of these
    // carries `disabled={!securityOperator}` — the screen's mirror of the
    // securityOps middleware that actually refuses the write.
    await navTo(page, "Governance");
    await expect(page.getByRole("button", { name: GOV.NEW_CTA, exact: true })).toBeEnabled();
    await expect(page.getByRole("button", { name: `${GOV.EDIT} ${NAME}`, exact: true })).toBeEnabled();
    await expect(page.getByRole("button", { name: `${GOV.DELETE} ${NAME}`, exact: true })).toBeEnabled();
    await expect(page.getByRole("textbox", { name: PERM.FIELD_WHO, exact: true })).toBeEnabled();
    await expect(page.getByRole("button", { name: PERM.SUBJECT_GROUP, exact: true })).toBeEnabled();

    // Assign is READINESS-gated as well as tier-gated (assignments.tsx's
    // `ready`), so an empty form disables it for an admin too — fill it and
    // watch it arm. Deliberately NOT clicked: this block only asserts what the
    // tier is offered, and a second assignment row would be state the walk
    // above did not write.
    await page.getByRole("textbox", { name: PERM.FIELD_WHO, exact: true }).fill("some-other-group");
    await page.locator("#governance-assignment-profile").click();
    await page.getByRole("option", { name: NAME, exact: true }).click();
    await expect(page.getByRole("button", { name: GOV.ADD_CTA, exact: true })).toBeEnabled();
  });

  test("the audit feed is the org-wide trail, not the admin-only stub", async ({ page }) => {
    await gotoConsole(page, "admin");
    await navTo(page, "Audit");

    // audit.tsx:500 renders this ONLY for !securityOperator. Its absence over a
    // populated feed is the assertion — the security tier reads the same
    // unfiltered trail an admin does.
    await expect(page.getByText("The full audit feed is admin-only.")).toHaveCount(0);
    // The seeded backend has written runs, a secret and the profile above, so
    // the trail is genuinely non-empty — the absence above has to be measured
    // over real events, not over a screen that failed to load. The count badge
    // and the plain-language verbs are audit.spec.ts's own selectors (the
    // screen is a day-grouped stream, not a table).
    await expect(page.getByText(/^\d+ events?$/)).toBeVisible();
    await expect(page.locator("main").getByText("Stored a secret")).toBeVisible();
  });

  test("an egress decision is theirs to make — Approve and Deny are both live", async ({ page }) => {
    const runId = sql("SELECT id FROM agent_runs ORDER BY created_at LIMIT 1");
    expect(runId, "no seeded runs found — is the backend up and seeded?").not.toBe("");
    sql(`DELETE FROM approvals WHERE state = 'PENDING'`);
    sql(
      `INSERT INTO approvals (id, run_id, kind, requested_scope, state, requested_at)
       VALUES ('${randomUUID()}', '${runId}', 'egress_domain', '{"host":"github.example.com"}'::jsonb, 'PENDING', now())`,
    );

    await gotoConsole(page);
    await navToRoute(page, "/approvals");

    // authorizeUserDecision early-returns for isSecurityOperator
    // (approvals.go:392), so this tier decides ANY kind on ANY run org-wide —
    // and is never bounded by the egress_host capability a member would be.
    await expect(page.getByRole("button", { name: /^Approve$/ }).first()).toBeEnabled();
    await expect(page.getByRole("button", { name: /^Deny$/ }).first()).toBeEnabled();
  });

  test("but NOT the super admin's surfaces: writing a secret is refused, and it says why", async ({ page }) => {
    // M-2: org Secrets is super-admin only, so it is not in this tier's
    // Admin-view nav (packet M-A); their own namespace is the User view's.
    await gotoConsole(page, "admin");
    await expect(sidebarLink(page, "Secrets")).toHaveCount(0);
    await navToRoute(page, "/secrets");

    // Purely operator-gated (secrets.tsx:81,115) — the one chokepoint every
    // secret-write caller routes through.
    await expect(page.getByRole("button", { name: "Add secret" })).toBeDisabled();
    await expect(page.getByText(OPERATOR_ONLY_REASON, { exact: true })).toBeVisible();
  });

  test("real security admin receives a server refusal on secret writes", async ({ page }) => {
    // Ordinary secrets are self-service; the resident AWS credential names are
    // retired (no run reads them since 0.8), so nobody stores one and a
    // per-person write under one is refused as reserved.
    const path = "/api/v1/secrets/aws-access-key-id";
    const refused = await consoleAPI(page, "PUT", path, { value: "e2e-replacement-must-not-be-stored" });
    expect(refused.status, refused.text).toBe(403);
    expect(JSON.parse(refused.text)).toEqual({ error: "secret name is reserved for platform internals", reason: "secret_name_reserved" });
    expect(sql("SELECT count(*) FROM secrets WHERE name = 'aws-access-key-id'")).toBe("0");
  });

  test("nor reaching INTO a run they do not own: attach is not offered", async ({ page }) => {
    // A run created by someone else, live and interactive. `canAttach` is
    // owner-OR-admin (run-detail.tsx:548, run-detail-summary-header.tsx:88,
    // attach-terminal.tsx:307 — all three read the same predicate), so losing
    // the admin arm is exactly what this tier loses: deciding a verdict is not
    // opening a PTY.
    const runId = sql("SELECT id FROM agent_runs ORDER BY created_at OFFSET 2 LIMIT 1");
    sql(
      `UPDATE agent_runs SET created_by='someone.else@corp.example', state='RUNNING', interactive=true WHERE id='${runId}'`,
    );

    await gotoConsole(page);
    await page.goto(`/runs/${runId}`);

    await expect(page.getByText("Interactive", { exact: true })).toBeVisible();
    await expect(page.getByText("Interactive — attachable")).toHaveCount(0);
  });
});

test.describe("governance — admin (unmocked, the harness's real session): the negative control", () => {
  test("the SAME screens the security admin was refused are live for a super admin", async ({ page }) => {
    await gotoConsole(page, "admin");

    // Without this pair the two assertions above would pass just as well on a
    // screen that is broken for everyone.
    await expect(sidebarLink(page, "Governance")).toBeVisible();
    await navTo(page, "Secrets");
    await expect(page.getByRole("button", { name: "Add secret" })).toBeEnabled();
    await expect(page.getByText(OPERATOR_ONLY_REASON)).toHaveCount(0);

    const runId = sql("SELECT id FROM agent_runs WHERE created_by='someone.else@corp.example' LIMIT 1");
    await page.goto(`/runs/${runId}`);
    await expect(page.getByText("Interactive — attachable")).toBeVisible();
  });
});

// ---------------------------------------------------------------------------
// 5. The §E "yolo" walls, at the API level.
//
// The north-star scenario is a group whose profile lets an agent run
// autonomously but is hard-walled anyway, and whose developer cannot loosen any
// of it. The LIVE RUN half is not achievable on this harness (see the header:
// no IdP, and the bearer caller is an operator, so effectiveCeiling never
// reads the store for it). What IS reachable is asserted here against the real
// backend: the walls as stored, the resolver that binds the group to them, and
// the two refusals that stop a widening.
// ---------------------------------------------------------------------------

test.describe("governance — the walls, asserted where this harness can reach them", () => {
  const auth = { Authorization: `Bearer ${ADMIN_TOKEN}` };

  test("the stored ceiling IS the wall: hosts denied, barrier floored, zero grant kinds", async ({ page }) => {
    const res = await page.request.get("/api/v1/governance", { headers: auth });
    expect(res.status()).toBe(200);
    const snap = await res.json();
    const profile = snap.profiles.find((p: { name: string }) => p.name === NAME);
    expect(profile, `profile ${NAME} missing — did the authoring walk run?`).toBeTruthy();

    // Autonomous: every tool call is allowed, nothing parks for a human.
    expect(profile.ceiling.tool_rules).toEqual([{ tool: "*", effect: "allow" }]);
    // …and hard-walled anyway.
    expect(profile.ceiling.denied_domains).toEqual(["github.example.com", "artifactory.corp.example.com"]);
    expect(profile.ceiling.min_confinement_class).toBe("CC3");
    // The structural half: no eligible grant kind under this ceiling at all,
    // so there is nothing for the proxy to mint toward those hosts. Absent or
    // empty are the same statement (`omitempty` on the wire).
    expect(profile.ceiling.eligible_grants ?? []).toEqual([]);
    // And the launch modes a ceiling cannot reach are denied beside it.
    expect(profile.limits).toEqual({ deny_task_mode_exec: true, deny_interactive: true });
  });

  test("the resolver binds the group to it — and a user assignment beats the group", async ({ page }) => {
    const preview = async (body: unknown) => {
      const res = await page.request.post("/api/v1/governance/preview", { headers: auth, data: body });
      expect(res.status()).toBe(200);
      return res.json();
    };

    // THE SAME CALL every real run makes: handlePreviewGovernanceProfile runs
    // Store.ResolveGovernanceProfile, which is what effectiveCeiling takes on
    // the enforcement path. There is no second implementation of precedence.
    expect(await preview({ user_subjects: [], groups: [GROUP] })).toMatchObject({
      profile_name: NAME,
      matched_tier: "group",
    });

    // Give the same person a user-tier row pointing at a second profile.
    const wide = await page.request.post("/api/v1/governance/profiles", {
      headers: auth,
      data: {
        name: "less-walled",
        ceiling: { ...YOLO_CEILING, denied_domains: [], min_confinement_class: "CC1" },
        limits: {},
      },
    });
    expect(wide.status()).toBe(201);
    const wideID = (await wide.json()).profile.id;
    const assigned = await page.request.post("/api/v1/governance/assignments", {
      headers: auth,
      data: { subject_type: "user", subject: USER, profile_id: wideID, priority: 0 },
    });
    expect(assigned.ok()).toBeTruthy();

    // The most specific assignment wins — a person beats a group, whatever the
    // group's priority. This is GOV.PRECEDENCE, enforced rather than described.
    expect(await preview({ user_subjects: [USER], groups: [GROUP] })).toMatchObject({
      profile_name: "less-walled",
      matched_tier: "user",
    });
    // …and the group row still binds everyone else in it.
    expect(await preview({ user_subjects: ["other@corp.example"], groups: [GROUP] })).toMatchObject({
      profile_name: NAME,
      matched_tier: "group",
    });
  });

  test("the walls cannot be loosened: the grant bound refuses, and DELETE refuses too", async ({ page }) => {
    // A profile may force approval ON, never off — the monotone-⊆ bound
    // (governance_grantbound.go). This is the same refusal the editor renders,
    // asserted here on the WIRE so the discriminator prefix itself is pinned.
    const refused = await page.request.post("/api/v1/governance/profiles", {
      headers: auth,
      data: { name: "loosened", ceiling: OVERWIDE_CEILING, limits: {} },
    });
    expect(refused.status()).toBe(400);
    expect((await refused.json()).error).toContain('invalid ceiling: eligible grant "github_token"');

    // And the supported way OUT of a ceiling is removing the assignment, never
    // deleting the profile out from under its subjects: ON DELETE RESTRICT.
    const snap = await (await page.request.get("/api/v1/governance", { headers: auth })).json();
    const id = snap.profiles.find((p: { name: string }) => p.name === NAME).id;
    const del = await page.request.delete(`/api/v1/governance/profiles/${id}`, { headers: auth });
    expect(del.status()).toBe(409);
    const body = (await del.json()).error as string;
    expect(body).toContain("still assigned");
    // COUNT-FREE, deliberately: the client believed the count was zero on this
    // path, so the shipped 409 must not grow an n (§7.4).
    expect(body).not.toMatch(/\d+ assignment/);
  });
});

// ---------------------------------------------------------------------------
// 4. Composition (0.8.6) — a base, and a child that narrows it.
// ---------------------------------------------------------------------------

test.describe("governance — a profile that narrows a base", () => {
  const BASE_NAME = "compose-base";
  const CHILD_NAME = "compose-child";
  const auth = { Authorization: `Bearer ${ADMIN_TOKEN}` };

  test("creates a base and a child, narrows a domain and a limit, and sees the effective view", async ({ page }) => {
    await gotoConsole(page, "admin");
    await navTo(page, "Governance");

    // The base is an ordinary standalone profile: two domains and a quota of five.
    await page.getByRole("button", { name: GOV.NEW_CTA, exact: true }).click();
    let editor = page.getByTestId("governance-profile-editor");
    await page.locator("#governance-profile-name").fill(BASE_NAME);
    await editor.getByRole("button", { name: "Edit policy", exact: true }).click();
    await editor.getByLabel(SPEC_LABEL).fill(
      JSON.stringify({
        allowed_domains: ["a.example.com", "b.example.com"],
        first_use_approval: "deny_with_review",
        min_confinement_class: "CC2",
      }),
    );
    await page.locator("#governance-limit-concurrent").fill("5");
    await page.getByRole("button", { name: GOV.SAVE, exact: true }).click();
    await expect(profilesTable(page).getByRole("cell", { name: BASE_NAME, exact: true })).toBeVisible();

    // The child picks it as its base. The JSON editor gives way to overlay rows
    // that show what is inherited.
    await page.getByRole("button", { name: GOV.NEW_CTA, exact: true }).click();
    editor = page.getByTestId("governance-profile-editor");
    await page.locator("#governance-profile-name").fill(CHILD_NAME);
    await page.locator("#governance-profile-base").click();
    await page.getByRole("option", { name: BASE_NAME }).click();
    await expect(editor.getByText(GOV.OVERLAY_LEAD)).toBeVisible();
    await expect(editor.getByText(GOV.OVERLAY_INHERITED("a.example.com, b.example.com"))).toBeVisible();

    // Narrow one domain and the quota; leave everything else inherited.
    await editor.getByRole("switch", { name: `allowed_domains ${GOV.OVERLAY_NARROW}` }).click();
    await editor.getByLabel("allowed_domains", { exact: true }).fill("a.example.com");
    await editor.getByRole("switch", { name: `${GOV.LIMIT_CONCURRENT_LABEL} ${GOV.OVERLAY_NARROW}` }).click();
    await editor.getByLabel(GOV.LIMIT_CONCURRENT_LABEL, { exact: true }).fill("2");
    await page.getByRole("button", { name: GOV.SAVE, exact: true }).click();

    // The list marks it composed and names its base.
    const row = profilesTable(page).getByRole("row", { name: new RegExp(CHILD_NAME) });
    await expect(row.getByText(GOV.BASE_CHIP(BASE_NAME))).toBeVisible();

    // A real write: only the narrowed fields are stored.
    const snap = await (await page.request.get("/api/v1/governance", { headers: auth })).json();
    const child = snap.profiles.find((p: { name: string }) => p.name === CHILD_NAME);
    expect(child.overlay).toEqual({ allowed_domains: ["a.example.com"] });
    expect(child.overlay_limits).toEqual({ max_concurrent_runs: 2 });
    expect(child.base_profile_id).toBeTruthy();

    // Reopened, the effective view is the base narrowed: one domain, a quota of two.
    await row.getByRole("button", { name: `${GOV.EDIT} ${CHILD_NAME}` }).click();
    const view = page.getByTestId("governance-profile-effective");
    await expect(view.getByText(GOV.EFFECTIVE_TITLE)).toBeVisible();
    await expect(view.getByTestId("governance-effective-allowed_domains")).toHaveText("a.example.com");
    await expect(view.getByText(GOV.LIMIT_QUOTA_LABEL(2))).toBeVisible();
    await page.getByRole("button", { name: PEOPLE.CANCEL, exact: true }).click();

    // The base cannot be deleted from under its child.
    await profilesTable(page).getByRole("button", { name: `${GOV.DELETE} ${BASE_NAME}` }).click();
    const dialog = page.getByRole("alertdialog");
    await expect(dialog.getByText(GOV.REFUSED_HAS_CHILDREN)).toBeVisible();
    await expect(dialog.getByRole("button", { name: GOV.DELETE, exact: true })).toBeDisabled();
  });
});
