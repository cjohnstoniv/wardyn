/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import {
  test,
  expect,
  ADMIN_TOKEN,
  gotoConsole,
  mockMemberRole,
  mockSecurityAdminRole,
  navTo,
  navToRoute,
  sidebarLink,
} from "./fixtures";
import { DRIVES, DRIVE_MEMBER, PEOPLE, PERM, PREVIEW } from "../src/app/lib/user-drives-copy";
import { GOVERNANCE as GOV } from "../src/app/lib/governance-copy";
import type { MeUserDrive } from "../src/app/lib/api/health";
import type { Page } from "@playwright/test";

// ---------------------------------------------------------------------------
// User drives e2e (0.7) — lane: drives, port 8288, db wardyn_e2e.
//
// Every expected string is IMPORTED from lib/user-drives-copy.ts (which
// re-exports the §7.1 canon it reuses — PERM/PEOPLE/PREVIEW — so this file has
// ONE import site for drive copy) plus governance-copy.ts for the two strings
// §7.1 deliberately leaves at their own home: the priority column and the
// preview's "not saved" footer, and the door's own LIMIT_DRIVE_LABEL. Nothing
// here retypes a sentence: a copy change must break this spec rather than let
// the screen drift away from docs/design/user-drives-prompt.md §7.
//
// THE HARNESS CEILING, AND WHY HALF THIS FILE IS GUARDED
// -----------------------------------------------------
// scripts/e2e-backend.sh boots wardynd with `-runner none`, and
// buildRunnerFromFlags (cmd/wardynd/boot_deps.go:171) hard-codes
// RunnerTarget = "none" for that selection — there is no flag or env that sets
// the target independently of `-runner` (the only knob is -runner /
// WARDYN_RUNNER). types.ValidateUserDrive then requires
// backend.RunnerTarget() == cfg.RunnerTarget, so on THIS deployment every one
// of the four backends is refused and POST /api/v1/drives answers 400 for all
// of them. Verified live against :8288:
//
//   GET  /api/v1/drives -> {"drives":[],"grants":[],
//                           "host_roots_configured":false,"runner_target":"none"}
//   POST /api/v1/drives -> 400 invalid drive: backend "docker_volume" cannot be
//                          mounted by this deployment's runner (none)
//
// No drive row can exist here by ANY route — API or browser — and everything
// downstream of one (an allocation, the resolver's preview, /me.user_drive)
// is unreachable with it. So this file splits in two:
//
//   1. What a runner-less deployment CAN prove, driven for real and unmocked:
//      the screen over an empty registry, the editor offering no backend and
//      rendering the SERVER's own refusal verbatim, the three roles' entry
//      points, the governance door (profiles ARE writable here), the member's
//      absent-drive arm, and — the strongest leg — a member ticking the
//      checkbox and the REAL server answering the frozen REFUSED_NO_GRANT,
//      which is itself proof the console put `drive` on the wire.
//
//   2. The authoring walk, which needs a drive to exist: allocation, the
//      delete refusal, the resolved preview and the backend picker's two
//      offered options. Guarded by skipUnlessMountable(), which reads the
//      SERVER's own runner_target — so the day the harness boots with a
//      mounting target these run UNMODIFIED against the real daemon, seeding
//      through the API exactly as governance.spec.ts does. They SKIP today,
//      naming the cause. They are the one part of this file the current gate
//      does not execute.
//
// What is spliced, and why, in each place:
//
//   THE THREE ROLES. mockMemberRole / mockSecurityAdminRole splice /me only,
//   so these prove RENDER behaviour — server-side authorization is pinned in
//   Go (the operatorOnly route group, authz_test.go). fixtures.ts's own
//   documented ceiling, not a shortcut taken here.
//
//   /me.user_drive. A member's allocation cannot exist on this backend (see
//   above), so the three display moments are spliced onto the REAL /me
//   response — route.fetch() + patch + refulfill, the technique
//   fixtures.ts's mockMemberRole documents, so the shape around it stays
//   genuine. The ABSENT arm needs no splice and is asserted unmocked, because
//   the real backend genuinely answers user_drive:null for this caller. The
//   resolver that fills the field is pinned server-side
//   (internal/api/user_drives_test.go, internal/types/user_drive_test.go).
//
// SERIAL IS PER-BLOCK, not per-file. Only two blocks below build state across
// their own tests — the governance door (a profile one test writes and the
// next reads) and the authoring walk — and those two declare it themselves.
// Configuring it for the whole file would make the first failure cascade into
// a dozen silent skips, which is exactly how this file's first gate run hid
// twelve unrun tests behind one broken assertion.

const auth = { Authorization: `Bearer ${ADMIN_TOKEN}` };

// The authoring walk's objects. USER is a literal because there IS no seeded
// member identity in this harness: the bearer token authenticates as
// "admin-token" and isOperator reads "no session role to demote" for any
// caller with no OIDC human session, so a subject to allocate to has to be
// named rather than discovered — the same choice governance.spec.ts makes for
// its own assignment subject.
const NAME = "team-scratch";
const RENAMED = "team-scratch-2";
const USER = "contractor@corp.example";

// The member's allocation as /me would carry it. 10240 MiB is a whole multiple
// of 1024, so driveSizeLabel spells it "10 GiB" — the GIB arm of the one size
// helper, which a MiB-sized fixture would leave unexercised.
const ALLOCATED: MeUserDrive = {
  name: NAME,
  backend: "docker_volume",
  size_mib: 10240,
  writable: true,
  enforcement: "none",
  home_name: "a1b2c3d4e5",
};
const SIZE = DRIVES.SIZE_GIB(10);

// CONSOLE-RULES §6 — exactly ONE `default`-variant (teal) button on the screen
// at any moment. The default variant is `bg-primary` (ui/button.tsx), and the
// editor's Writable / the form's Enabled switches paint bg-primary too when
// checked (form-primitives.tsx) — excluded, because a toggle is not an action.
// Scoped to <main>: the app shell's own "New run" in the top bar is a default
// button on every screen and is not this surface's.
const tealActions = (page: Page) => page.locator("main").locator('button.bg-primary:not([role="switch"])');

// The screen draws TWO tables once there is anything in them — the drives
// list's Name column and the allocations list's Drive column — and a drive
// NAME is a cell in both. Every row assertion says which one it means.
const drivesTable = (page: Page) => page.getByRole("table").first();
const allocationsTable = (page: Page) => page.getByRole("table").nth(1);

// The editor's four long option labels; on a runner-less deployment NONE of
// them is offered, which is what the refusal block asserts.
const BACKEND_LABELS = [
  DRIVES.BACKEND_DOCKER_VOLUME,
  DRIVES.BACKEND_HOST_PATH,
  DRIVES.BACKEND_K8S_PVC,
  DRIVES.BACKEND_K8S_PVC_STATIC,
];

// Reach /drives the way the product does: it has NO nav item, and the
// Workspaces header's outline button is the one door from a nav-bearing
// screen (user-drives-prompt.md §6). Using it here means the entry point is
// exercised by every test that needs the screen, not only by the one that
// asserts it.
async function gotoDrives(page: Page): Promise<void> {
  await gotoConsole(page);
  await navTo(page, "Workspaces");
  await page.getByRole("button", { name: DRIVES.TITLE, exact: true }).click();
  await expect(page.getByRole("heading", { name: DRIVES.TITLE, level: 1 })).toBeVisible();
}

// Skip a test that needs a drive to EXIST. Reads the deployment's own answer
// rather than a hard-coded flag, so this file needs no edit the day the
// harness boots with a mounting runner target — see the header.
async function skipUnlessMountable(page: Page): Promise<void> {
  const res = await page.request.get("/api/v1/drives", { headers: auth });
  expect(res.status(), "GET /api/v1/drives should answer for the admin bearer").toBe(200);
  const target = ((await res.json()) as { runner_target?: string }).runner_target ?? "";
  // The two substrates DriveBackend.RunnerTarget() can name. Anything else —
  // "none" above all — refuses every backend at validateUserDrive.
  test.skip(
    target !== "docker" && target !== "k8s",
    `this deployment's runner_target is "${target}", so validateUserDrive refuses every backend and no drive can be created — boot the e2e daemon with a mounting runner target to run the authoring walk`,
  );
}

// ONE handler, deliberately not fixtures.ts's mockMemberRole plus a second
// route: Playwright runs the most recently registered matching handler FIRST,
// and this one answers with route.fetch(), which goes to the NETWORK rather
// than to the other handler — so a second route on /me would silently drop
// whichever patch it did not itself carry. The three role fields are exactly
// mockMemberRole's; user_drive and the door ride beside them.
async function mockMemberDrive(page: Page, drive: MeUserDrive | null, deniedBy = ""): Promise<void> {
  await page.route("**/api/v1/me", async (route) => {
    const response = await route.fetch();
    const json = await response.json();
    json.role = "member";
    json.operator = false;
    json.security_operator = false;
    json.user_drive = drive;
    json.user_drive_denied_by_profile = deniedBy;
    await route.fulfill({ response, json });
  });
}

// The run request the console actually put on the wire. Captured and PASSED
// THROUGH (route.continue), so the real server still answers — the launch
// refusal below is the server's, not a stub's.
type LaunchBody = { drive?: { enabled: boolean; read_only?: boolean } };

async function watchLaunch(page: Page): Promise<{ body: LaunchBody | null }> {
  const seen: { body: LaunchBody | null } = { body: null };
  await page.route("**/api/v1/runs**", async (route) => {
    if (route.request().method() === "POST") seen.body = route.request().postDataJSON() as LaunchBody;
    await route.continue();
  });
  return seen;
}

// ---------------------------------------------------------------------------
// 1. The registry over a deployment that dispatches nowhere — unmocked.
// ---------------------------------------------------------------------------

test.describe("drives — an empty registry says what it is, not what went wrong", () => {
  test("the screen's standing surface: both empty states, the two notes, and ONE teal", async ({ page }) => {
    await gotoDrives(page);

    // Rendered through withMono (the mount target is a literal), so the string
    // is split across a <p> and a mono <span> — the <p> is still the smallest
    // element carrying all of it.
    await expect(page.getByText(DRIVES.LEAD)).toBeVisible();
    await expect(page.getByText(DRIVES.DRIVES_TITLE, { exact: true })).toBeVisible();
    await expect(page.getByText(DRIVES.DRIVES_LEAD)).toBeVisible();

    // Empty, not unconfigured: there is deliberately no "drives not configured"
    // state (§7.4), so a fresh install reads as a feature nobody has filled in.
    await expect(page.getByText(DRIVES.EMPTY_TITLE)).toBeVisible();
    await expect(page.getByText(DRIVES.EMPTY_BODY)).toBeVisible();
    await expect(page.getByText(DRIVES.FETCH_FAILED_TITLE)).toHaveCount(0);

    // The second empty state, and the standing facts that outlive it.
    await expect(page.getByText(DRIVES.EMPTY_ALLOC_TITLE)).toBeVisible();
    await expect(page.getByText(DRIVES.EMPTY_ALLOC_BODY)).toBeVisible();
    await expect(page.getByText(DRIVES.PRECEDENCE)).toBeVisible();
    // Two halves of ONE fact, and they render together: an allocation binds at
    // the next RUN, a group membership only at the next SIGN-IN.
    await expect(page.getByText(DRIVES.EFFECT_NOTE)).toBeVisible();
    await expect(page.getByText(DRIVES.SIGNIN_NOTE)).toBeVisible();

    // ONE teal, and with no drives it is the empty state's own New drive —
    // never the allocation form's Allocate, which has no drive to bind.
    await expect(tealActions(page)).toHaveCount(1);
    await expect(page.getByRole("button", { name: DRIVES.NEW_CTA, exact: true })).toHaveClass(/bg-primary/);
  });

  test("the preview is offered but parked: nothing is allocated, so there is nothing to resolve", async ({
    page,
  }) => {
    await gotoDrives(page);

    await expect(page.getByText(DRIVES.PREVIEW_TITLE, { exact: true })).toBeVisible();
    await expect(page.getByText(DRIVES.PREVIEW_LEAD)).toBeVisible();
    // The People step's own field and hint, verbatim — one accepted shape.
    await expect(page.getByText(PREVIEW.FIELD_CLAIMS, { exact: true })).toBeVisible();
    await expect(page.getByText(PREVIEW.FIELD_CLAIMS_HINT)).toBeVisible();
    // The footer is governance's, reused rather than re-frozen (§7.1).
    await expect(page.getByText(GOV.PREVIEW_NOT_SAVED)).toBeVisible();

    // Disabled rather than offered-and-answered-with-nothing: with no
    // allocation every answer would be PREVIEW_NONE, which teaches nothing.
    await expect(page.getByRole("button", { name: DRIVES.PREVIEW_CTA, exact: true })).toBeDisabled();
    await page.locator("#drive-preview-claims").fill("eng-contractors");
    await expect(page.getByRole("button", { name: DRIVES.PREVIEW_CTA, exact: true })).toBeDisabled();
  });

  test("the deployment facts the console cannot derive come off the wire", async ({ page }) => {
    const res = await page.request.get("/api/v1/drives", { headers: auth });
    expect(res.status()).toBe(200);
    const snap = await res.json();

    // Both lists are present and empty — nil Go slices encode as null, and the
    // client coerces, so an empty ARRAY here is the contract the screen maps
    // over.
    expect(snap.drives).toEqual([]);
    expect(snap.grants).toEqual([]);
    // The two scalars the backend picker is built from. A boolean, never the
    // roots themselves: a host path an admin may not read is not a hint.
    expect(typeof snap.host_roots_configured).toBe("boolean");
    expect(typeof snap.runner_target).toBe("string");
  });
});

// ---------------------------------------------------------------------------
// 2. The editor over that same deployment — the refusal is the SERVER's.
// ---------------------------------------------------------------------------

test.describe("drives — the editor refuses what this deployment cannot mount", () => {
  test("it opens IN PLACE, and the allocation form collapses and takes its teal with it", async ({ page }) => {
    await gotoDrives(page);
    await page.getByRole("button", { name: DRIVES.NEW_CTA, exact: true }).click();

    // Under the list it edits — not a dialog.
    const editor = page.getByTestId("drives-drive-editor");
    await expect(editor).toBeVisible();
    await expect(page.getByRole("alertdialog")).toHaveCount(0);
    await expect(editor.getByRole("heading", { name: DRIVES.EDITOR_TITLE_NEW })).toBeVisible();
    await expect(editor.getByText(DRIVES.NAME_HINT)).toBeVisible();
    await expect(editor.getByText(DRIVES.BACKEND_HINT)).toBeVisible();
    await expect(editor.getByText(DRIVES.HOME_HINT)).toBeVisible();
    // The rule renders under the directory field for EVERY option, because it
    // is what a claim is measured against whichever template names it.
    await expect(editor.getByText(DRIVES.HOME_RULE)).toBeVisible();
    await expect(editor.getByText(DRIVES.WRITABLE_HINT)).toBeVisible();
    await expect(editor.getByText(DRIVES.RECLAIM_HINT)).toBeVisible();

    // The collapse: a disabled summary row where the add form was — DISABLED,
    // not removed (§7's "a busy control is disabled, not removed").
    await expect(page.getByTestId("drives-add-allocation-collapsed")).toBeVisible();
    const allocate = page.getByRole("button", { name: DRIVES.ADD_CTA, exact: true });
    await expect(allocate).toBeDisabled();
    await expect(allocate).not.toHaveClass(/bg-primary/);

    // ONE default-weight action on the surface, and it is Save drive.
    await expect(tealActions(page)).toHaveCount(1);
    await expect(page.getByRole("button", { name: DRIVES.SAVE_CTA, exact: true })).toHaveClass(/bg-primary/);
    // Save waits for the one field that has no default.
    await expect(page.getByRole("button", { name: DRIVES.SAVE_CTA, exact: true })).toBeDisabled();

    await page.getByRole("button", { name: PEOPLE.CANCEL, exact: true }).click();
    await expect(page.getByTestId("drives-drive-editor")).toHaveCount(0);
    await expect(page.getByTestId("drives-add-allocation-collapsed")).toHaveCount(0);
  });

  test("a runner that mounts nothing is offered NO backend — not a picker whose every save 400s", async ({
    page,
  }) => {
    await gotoDrives(page);
    await page.getByRole("button", { name: DRIVES.NEW_CTA, exact: true }).click();
    const editor = page.getByTestId("drives-drive-editor");

    // backendsFor() answers with the pair THIS runner can mount and nothing
    // else, so an unrecognised target offers none rather than guessing a pair
    // whose every save would be refused. On a docker deployment this is where
    // the k8s pair's absence is asserted; here the whole set is absent, which
    // is the same rule with a stronger input.
    await expect(editor.getByText(DRIVES.FIELD_BACKEND, { exact: true })).toBeVisible();
    for (const label of BACKEND_LABELS) {
      await expect(editor.getByRole("button", { name: label })).toHaveCount(0);
    }
    // …and with no backend chosen, neither of the backend-specific fields is
    // drawn: they follow the selection, not the screen.
    await expect(page.locator("#drive-host-root")).toHaveCount(0);
    await expect(page.locator("#drive-storage-class")).toHaveCount(0);
  });

  test("Save renders the SERVER's own refusal under the console's heading, and writes nothing", async ({
    page,
  }) => {
    await gotoDrives(page);
    await page.getByRole("button", { name: DRIVES.NEW_CTA, exact: true }).click();
    const editor = page.getByTestId("drives-drive-editor");

    await page.locator("#drive-name").fill("refused-by-the-runner");
    await expect(page.getByRole("button", { name: DRIVES.SAVE_CTA, exact: true })).toBeEnabled();
    await page.getByRole("button", { name: DRIVES.SAVE_CTA, exact: true }).click();

    // The heading is the console's; the body is the SERVER's prose, which
    // names the backend AND the runner — both of which a frozen sentence would
    // have had to drop (§7.1's "deliberately absent" table).
    await expect(editor.getByText(DRIVES.SAVE_REFUSED_TITLE)).toBeVisible();
    await expect(editor.getByText(/cannot be mounted by this deployment's runner/)).toBeVisible();
    // SAVE_ERROR is the unreachable-server arm, not a refusal — a 400 must
    // never render it.
    await expect(editor.getByText(DRIVES.SAVE_ERROR)).toHaveCount(0);

    // Nothing was written: the editor stays open over the refusal and the list
    // behind it is still empty.
    await expect(editor).toBeVisible();
    await page.getByRole("button", { name: PEOPLE.CANCEL, exact: true }).click();
    await expect(page.getByText(DRIVES.EMPTY_TITLE)).toBeVisible();
  });

  test("the same refusal on the WIRE, so the status and the message itself are pinned", async ({ page }) => {
    const res = await page.request.post("/api/v1/drives", {
      headers: auth,
      data: { name: "refused-on-the-wire", backend: "docker_volume", home_template: "hash", reclaim: "retain" },
    });
    // A statement about THIS deployment's substrate, so a 400 at the write
    // rather than a 422 on somebody's run three days later.
    expect(res.status()).toBe(400);
    expect((await res.json()).error).toMatch(/cannot be mounted by this deployment's runner/);

    // And the registry is still empty — the refusal is not a partial write.
    const snap = await (await page.request.get("/api/v1/drives", { headers: auth })).json();
    expect(snap.drives).toEqual([]);
  });
});

// ---------------------------------------------------------------------------
// 3. The door. A security admin's whole authority over drives is this row —
//    /drives itself is SUPER — so it lives in the governance profile editor
//    and its two strings live in governance-copy.ts, never in the drives
//    module (user-drives-prompt.md §5 #5).
// ---------------------------------------------------------------------------

test.describe("drives — the door is a governance limit, not a drives control", () => {
  // The second test reads the profile the first one writes.
  test.describe.configure({ mode: "serial" });
  const DOOR = "no-drives-here";

  test("the profile editor's third limit shuts it, and the profiles table chips it", async ({ page }) => {
    await gotoConsole(page);
    await navTo(page, "Governance");
    await page.getByRole("button", { name: GOV.NEW_CTA, exact: true }).click();

    const editor = page.getByTestId("governance-profile-editor");
    await expect(editor.getByText(GOV.LIMITS_LEAD)).toBeVisible();
    await page.locator("#governance-profile-name").fill(DOOR);

    // The third LimitRow, beside the two launch modes — and its hint says what
    // it overrides: an allocation the admin already made.
    const door = editor.getByRole("switch", { name: GOV.LIMIT_DRIVE_LABEL });
    await expect(door).toHaveAttribute("aria-checked", "false");
    await expect(editor.getByText(GOV.LIMIT_DRIVE_HINT)).toBeVisible();
    await door.click();
    await expect(door).toHaveAttribute("aria-checked", "true");

    await page.getByRole("button", { name: GOV.SAVE, exact: true }).click();

    // The chip, beside the other two limits' (user-drives mock, state 6).
    const row = page.getByRole("table").first().getByRole("row", { name: new RegExp(DOOR) });
    await expect(row.getByText(GOV.LIMIT_DRIVE_LABEL)).toBeVisible();
    await expect(row.getByText(GOV.LIMITS_NONE)).toHaveCount(0);

    // It was a real write, not local state.
    await page.reload();
    await expect(
      page.getByRole("table").first().getByRole("row", { name: new RegExp(DOOR) }).getByText(GOV.LIMIT_DRIVE_LABEL),
    ).toBeVisible();
  });

  test("the stored limit is the door: deny_user_drive on the profile itself", async ({ page }) => {
    const snap = await (await page.request.get("/api/v1/governance", { headers: auth })).json();
    const profile = snap.profiles.find((p: { name: string }) => p.name === DOOR);
    expect(profile, `profile ${DOOR} missing — did the door test run?`).toBeTruthy();
    // The limits object is the whole statement: the drive door shut, and
    // neither launch mode touched by it.
    expect(profile.limits).toEqual({ deny_user_drive: true });
  });
});

// ---------------------------------------------------------------------------
// 4. Who may reach the registry. /me is spliced for the ROLE only; server-side
//    authorization is Go's (the operatorOnly route group).
// ---------------------------------------------------------------------------

test.describe("drives — the registry is SUPER's, and it has no nav item for anyone", () => {
  test("an admin reaches it from Workspaces and from the Settings card — and never from the sidebar", async ({
    page,
  }) => {
    await gotoConsole(page);
    // There is no Drives nav entry at all: the sidebar's NAV_ITEMS never grew
    // one, on purpose (§6) — the two entry points below are the whole door.
    await expect(page.getByRole("link", { name: new RegExp(`^${DRIVES.TITLE}`) })).toHaveCount(0);

    await navTo(page, "Workspaces");
    // `outline`, never teal: the teal stays on Add workspace, because a drive
    // is allocated rather than onboarded.
    const door = page.getByRole("button", { name: DRIVES.TITLE, exact: true });
    await expect(door).toBeVisible();
    await expect(door).not.toHaveClass(/bg-primary/);
    await door.click();
    await expect(page).toHaveURL(/\/drives$/);

    // The second home of the same component (setup step + Settings).
    await navToRoute(page, "/settings");
    const card = page.getByTestId("user-drives-card");
    await expect(card).toBeVisible();
    await expect(card.getByText(DRIVES.CARD_LEAD)).toBeVisible();
    // It counts ALLOCATIONS, never people — and with none it says so rather
    // than rendering a zero.
    await expect(card.getByText(DRIVES.CARD_EMPTY)).toBeVisible();
    await card.getByText(DRIVES.CARD_OPEN).click();
    await expect(page).toHaveURL(/\/drives$/);
  });

  test("a security admin is offered neither door, and every write on the screen is parked", async ({ page }) => {
    await mockSecurityAdminRole(page);
    await gotoConsole(page);

    // Their authority over drives is the governance door above, not the
    // registry — so they see no entry point at all.
    await navTo(page, "Workspaces");
    await expect(page.getByRole("button", { name: DRIVES.TITLE, exact: true })).toHaveCount(0);
    await navToRoute(page, "/settings");
    await expect(page.getByTestId("user-drives-card")).toHaveCount(0);

    // Reaching /drives directly renders the screen with its writes DISABLED —
    // the app's pattern for a tier-gated surface is a parked control, not a
    // refusal page (the same shape /governance uses for a non-securityOperator).
    await navToRoute(page, "/drives");
    await expect(page.getByRole("heading", { name: DRIVES.TITLE, level: 1 })).toBeVisible();
    await expect(page.getByRole("button", { name: DRIVES.NEW_CTA, exact: true })).toBeDisabled();
    // The surface still has exactly ONE default-weight action — the teal is a
    // property of the SCREEN's shape (drives-screen.tsx's `teal`), not of the
    // viewer's tier — and for this tier it is parked. Asserting zero teals here
    // would be asserting a different screen.
    await expect(tealActions(page)).toHaveCount(1);
    await expect(tealActions(page)).toBeDisabled();
  });

  test("a member is offered neither door either", async ({ page }) => {
    await mockMemberRole(page);
    await gotoConsole(page);

    await navTo(page, "Workspaces");
    await expect(page.getByRole("button", { name: DRIVES.TITLE, exact: true })).toHaveCount(0);
    // Governance is not theirs either — the door that could shut their drive
    // is a screen they never see.
    await expect(sidebarLink(page, "Governance")).toHaveCount(0);
  });
});

// ---------------------------------------------------------------------------
// 5. The member's four states. Three are spliced onto the real /me (see the
//    header); the fourth — no allocation — is the real answer and is unmocked.
// ---------------------------------------------------------------------------

test.describe("drives — what the member is told at New run", () => {
  test("NO allocation and an open door: today's card, with no drive block at all", async ({ page }) => {
    // Unspliced: the absent-row doctrine. The real backend genuinely answers
    // user_drive:null for this caller, so this arm needs no mock — and a
    // member with nothing allocated must see the card exactly as it was
    // before the feature existed.
    await mockMemberRole(page);
    await gotoConsole(page);
    await navToRoute(page, "/runs/new");
    await expect(page.getByRole("heading", { name: "New run" })).toBeVisible();

    await expect(page.getByTestId("nr-drive")).toHaveCount(0);
    await expect(page.getByTestId("nr-drive-reason")).toHaveCount(0);
    await expect(page.getByText(DRIVE_MEMBER.NR_CHECKBOX)).toHaveCount(0);
    // Not even the "ask an admin" line: with no door shut there is nothing to
    // explain, and NR_NONE is a reason, not a placeholder.
    await expect(page.getByText(DRIVE_MEMBER.NR_NONE)).toHaveCount(0);
  });

  test("a WRITABLE allocation: the checkbox, its sentence, and the narrowing toggle", async ({ page }) => {
    await mockMemberDrive(page, ALLOCATED);
    await gotoConsole(page);
    await navToRoute(page, "/runs/new");

    const block = page.getByTestId("nr-drive");
    await expect(block).toBeVisible();
    await expect(block.getByText(DRIVE_MEMBER.NR_CHECKBOX)).toBeVisible();
    // The hint names the drive, the size and the mode of the MOUNT — and the
    // sentence after it promises persistence only because this mount is
    // writable.
    await expect(block.getByText(DRIVE_MEMBER.NR_HINT(NAME, SIZE, DRIVES.MODE_RW_INLINE))).toBeVisible();
    await expect(block.getByText(DRIVE_MEMBER.NR_RW_NOTE)).toBeVisible();
    await expect(block.getByText(DRIVE_MEMBER.NR_RO_NOTE)).toHaveCount(0);
    // Off by default: mounting is a choice per run, never a default.
    await expect(page.locator("#nr-drive-mount")).not.toBeChecked();

    // Only a writable allocation can be narrowed, and the toggle defaults OFF
    // (Q5) — a run may narrow what an admin granted, never widen it.
    const readOnly = page.locator("#nr-drive-readonly");
    await expect(readOnly).toBeVisible();
    await expect(readOnly).not.toBeChecked();
    await readOnly.click();

    // Narrowed FOR THIS RUN: the hint's mode and the sentence after it flip
    // together, so neither promises persistence a read-only mount cannot give.
    await expect(block.getByText(DRIVE_MEMBER.NR_HINT(NAME, SIZE, DRIVES.MODE_RO_INLINE))).toBeVisible();
    await expect(block.getByText(DRIVE_MEMBER.NR_RO_NOTE)).toBeVisible();
    await expect(block.getByText(DRIVE_MEMBER.NR_RW_NOTE)).toHaveCount(0);
  });

  test("a READ-ONLY allocation has no narrowing toggle — there is nothing to narrow", async ({ page }) => {
    await mockMemberDrive(page, { ...ALLOCATED, writable: false });
    await gotoConsole(page);
    await navToRoute(page, "/runs/new");

    await expect(page.getByTestId("nr-drive")).toBeVisible();
    await expect(page.getByText(DRIVE_MEMBER.NR_HINT(NAME, SIZE, DRIVES.MODE_RO_INLINE))).toBeVisible();
    await expect(page.getByText(DRIVE_MEMBER.NR_RO_NOTE)).toBeVisible();
    // Absent, not disabled: there is no matching control on a read-only
    // allocation to park.
    await expect(page.locator("#nr-drive-readonly")).toHaveCount(0);
    await expect(page.getByText(DRIVE_MEMBER.NR_READONLY_TOGGLE)).toHaveCount(0);
  });

  test("a PAUSED allocation is one sentence where the checkbox would be", async ({ page }) => {
    await mockMemberDrive(page, { ...ALLOCATED, paused: true });
    await gotoConsole(page);
    await navToRoute(page, "/runs/new");

    // An unmountable drive is not a disabled checkbox with a tooltip.
    await expect(page.getByTestId("nr-drive-reason")).toHaveText(DRIVE_MEMBER.NR_PAUSED);
    await expect(page.getByTestId("nr-drive")).toHaveCount(0);
    await expect(page.locator("#nr-drive-mount")).toHaveCount(0);
  });

  test("the DOOR outranks the allocation, and names the profile that shut it", async ({ page }) => {
    // Denied AND allocated: the door is checked first and independently, so
    // "ask an admin for an allocation" — the obvious advice — is never given
    // to someone whose profile is the actual reason.
    await mockMemberDrive(page, ALLOCATED, "walled");
    await gotoConsole(page);
    await navToRoute(page, "/runs/new");

    await expect(page.getByTestId("nr-drive-reason")).toHaveText(DRIVE_MEMBER.NR_DENIED("walled"));
    await expect(page.getByTestId("nr-drive")).toHaveCount(0);
    await expect(page.getByText(DRIVE_MEMBER.NR_CHECKBOX)).toHaveCount(0);
  });

  test("Getting Started carries the chip and the sentence that a drive is not a workspace", async ({ page }) => {
    await mockMemberDrive(page, ALLOCATED);
    await gotoConsole(page);
    await navToRoute(page, "/setup");

    await expect(page.getByRole("heading", { name: "What's set up for you" })).toBeVisible();
    await expect(page.getByText(DRIVE_MEMBER.GS_DRIVE_CHIP(NAME, SIZE, DRIVES.MODE_RW_INLINE))).toBeVisible();
    // Placed in the workspace card because that is the one place a drive and a
    // workspace get conflated — and the sentence says a drive is not one.
    await expect(page.getByText(DRIVE_MEMBER.GS_DRIVE_BODY)).toBeVisible();
  });

  test("…and with no allocation, Getting Started grows no chip and no placeholder", async ({ page }) => {
    await mockMemberRole(page);
    await gotoConsole(page);
    await navToRoute(page, "/setup");

    await expect(page.getByRole("heading", { name: "What's set up for you" })).toBeVisible();
    await expect(page.getByText(/^Drive · /)).toHaveCount(0);
    await expect(page.getByText(DRIVE_MEMBER.GS_DRIVE_BODY)).toHaveCount(0);
  });
});

// ---------------------------------------------------------------------------
// 6. The request reaches the server. THE strongest leg this harness can prove:
//    the console's wire shape is asserted on the way past, and the REAL server
//    answers the frozen §7.7 refusal — which it could only raise if `drive`
//    was actually on the request.
// ---------------------------------------------------------------------------

test.describe("drives — ticking the box puts `drive` on the wire", () => {
  test("mount ON sends drive.enabled, and the server answers the allocation refusal verbatim", async ({ page }) => {
    await mockMemberDrive(page, ALLOCATED);
    const seen = await watchLaunch(page);
    await gotoConsole(page);
    await navToRoute(page, "/runs/new");

    await page.locator("#nr-drive-mount").click();
    await expect(page.locator("#nr-drive-mount")).toBeChecked();
    await page.getByLabel("Title").fill("e2e drive mount");
    await page.getByRole("button", { name: "Launch run" }).click();

    // The refusal is the SERVER's own §7.7 string, rendered verbatim in the
    // launch rail — and this backend can only have raised it by reading a
    // `drive` flag off the request, because seedRequestDrive is a provable
    // no-op without one.
    await expect(page.getByText(DRIVE_MEMBER.REFUSED_NO_GRANT)).toBeVisible();

    // …and the shape the console put there: a bare FLAG. Nothing names a
    // drive, a path or a directory — the server resolves which drive is the
    // caller's (pkg/client.DriveSelection).
    expect(seen.body?.drive).toEqual({ enabled: true });
  });

  test("narrowing to read-only rides the same flag, as read_only", async ({ page }) => {
    await mockMemberDrive(page, ALLOCATED);
    const seen = await watchLaunch(page);
    await gotoConsole(page);
    await navToRoute(page, "/runs/new");

    await page.locator("#nr-drive-mount").click();
    await page.locator("#nr-drive-readonly").click();
    await page.getByLabel("Title").fill("e2e drive mount ro");
    await page.getByRole("button", { name: "Launch run" }).click();

    await expect(page.getByText(DRIVE_MEMBER.REFUSED_NO_GRANT)).toBeVisible();
    expect(seen.body?.drive).toEqual({ enabled: true, read_only: true });
  });

  test("untouched, the key is ABSENT — and the run launches, exactly as it did before 0.7", async ({ page }) => {
    // The negative control, and the absent-row doctrine on the wire: an
    // omitted `drive` is "mount nothing", byte for byte what every run sent
    // before this feature existed. Unmocked ADMIN session, which genuinely has
    // no allocation and therefore no checkbox to leave alone.
    const seen = await watchLaunch(page);
    await gotoConsole(page);
    await navToRoute(page, "/runs/new");
    await expect(page.getByRole("heading", { name: "New run" })).toBeVisible();
    // On the SCREEN THAT DRAWS IT — asserted before the launch, not from /runs,
    // where its absence would be true of every screen in the app.
    await expect(page.getByTestId("nr-drive")).toHaveCount(0);

    await page.getByLabel("Title").fill("e2e no drive");
    await page.getByRole("button", { name: "Launch run" }).click();
    await expect(page).toHaveURL(/\/runs\/[0-9a-f-]{8,}/, { timeout: 15_000 });
    expect(seen.body).not.toBeNull();
    expect(seen.body?.drive).toBeUndefined();
  });
});

// ---------------------------------------------------------------------------
// 7. The negative control: the member-only strings belong to the member.
// ---------------------------------------------------------------------------

test.describe("drives — an admin's New run says none of the member's sentences", () => {
  test("no checkbox, no reason line, no chip — the admin's own /me has no allocation", async ({ page }) => {
    await gotoConsole(page);
    await navToRoute(page, "/runs/new");
    await expect(page.getByRole("heading", { name: "New run" })).toBeVisible();

    // Without this the four member assertions above would pass just as well on
    // a screen that renders the block for everybody.
    await expect(page.getByTestId("nr-drive")).toHaveCount(0);
    await expect(page.getByTestId("nr-drive-reason")).toHaveCount(0);
    for (const s of [
      DRIVE_MEMBER.NR_CHECKBOX,
      DRIVE_MEMBER.NR_READONLY_TOGGLE,
      DRIVE_MEMBER.NR_RW_NOTE,
      DRIVE_MEMBER.NR_RO_NOTE,
      DRIVE_MEMBER.NR_PAUSED,
      DRIVE_MEMBER.NR_NONE,
    ]) {
      await expect(page.getByText(s)).toHaveCount(0);
    }

    // …and the surfaces that ARE the admin's are live on the same session.
    await navTo(page, "Workspaces");
    await expect(page.getByRole("button", { name: DRIVES.TITLE, exact: true })).toBeVisible();
  });
});

// ---------------------------------------------------------------------------
// 8. The authoring walk — GUARDED. See the header: no drive can exist on a
//    deployment whose runner mounts nothing, so every test below skips on this
//    harness and runs UNMODIFIED once the e2e daemon boots with a mounting
//    runner target. Seeding is through the API and the browser, never the DB.
// ---------------------------------------------------------------------------

test.describe("drives — the admin's authoring walk (needs a runner that can mount)", () => {
  // One drive, carried the length of the walk.
  test.describe.configure({ mode: "serial" });

  test.beforeEach(async ({ page }) => {
    await skipUnlessMountable(page);
  });

  test("registering a drive: it lands in the table, allocated to nobody", async ({ page }) => {
    await gotoDrives(page);
    await page.getByRole("button", { name: DRIVES.NEW_CTA, exact: true }).click();
    const editor = page.getByTestId("drives-drive-editor");

    await page.locator("#drive-name").fill(NAME);
    // The managed volume this runner can mount. Selected explicitly rather
    // than trusted to be the default, because the default is whatever the
    // deployment offers first.
    await editor.getByRole("button", { name: DRIVES.BACKEND_DOCKER_VOLUME }).click();
    await page.locator("#drive-size").fill("10240");
    await editor.getByRole("switch", { name: DRIVES.FIELD_WRITABLE }).click();
    await page.locator("#drive-reclaim-retain").click();
    await page.getByRole("button", { name: DRIVES.SAVE_CTA, exact: true }).click();

    // The row, in the table's own vocabulary: the kind chip over the wire
    // value, the size with its enforcement gloss, the mode as a WORD.
    const row = drivesTable(page).getByRole("row", { name: new RegExp(NAME) });
    await expect(row.getByRole("cell", { name: NAME, exact: true })).toBeVisible();
    await expect(row.getByText(DRIVES.KIND_MANAGED)).toBeVisible();
    await expect(row.getByText(SIZE)).toBeVisible();
    // A Docker managed volume has no byte cap, and the row says so on the
    // number rather than only in the note below the table.
    await expect(row.getByText(DRIVES.ENFORCEMENT_NONE)).toBeVisible();
    await expect(row.getByText(DRIVES.MODE_RW, { exact: true })).toBeVisible();
    await expect(row.getByText(DRIVES.RECLAIM_RETAIN)).toBeVisible();
    // Allocated to NOBODY — the zero state is a sentence, not a count.
    await expect(row.getByText(DRIVES.ALLOCATED_NONE)).toBeVisible();

    // The honesty note appears with the first row and is said ONCE.
    await expect(page.getByText(DRIVES.HONESTY)).toBeVisible();
    await expect(page.getByText(DRIVES.EMPTY_TITLE)).toHaveCount(0);

    // It was a real write.
    await page.reload();
    await expect(drivesTable(page).getByRole("cell", { name: NAME, exact: true })).toBeVisible();
  });

  test("the editor reopens on the SAVED drive, and a rename persists", async ({ page }) => {
    await gotoDrives(page);
    await page.getByRole("button", { name: `${DRIVES.EDIT} ${NAME}`, exact: true }).click();

    const editor = page.getByTestId("drives-drive-editor");
    await expect(editor.getByRole("heading", { name: DRIVES.EDITOR_TITLE_EDIT(NAME) })).toBeVisible();
    // Opened on what was stored, not on a fresh starter.
    await expect(page.locator("#drive-name")).toHaveValue(NAME);
    await expect(page.locator("#drive-size")).toHaveValue("10240");
    await expect(editor.getByRole("switch", { name: DRIVES.FIELD_WRITABLE })).toHaveAttribute("aria-checked", "true");

    await page.locator("#drive-name").fill(RENAMED);
    await page.getByRole("button", { name: DRIVES.SAVE_CTA, exact: true }).click();
    await expect(drivesTable(page).getByRole("cell", { name: RENAMED, exact: true })).toBeVisible();
  });

  test("a share cannot use the derived directory name — the refusal is avoided, not met", async ({ page }) => {
    await gotoDrives(page);
    await page.getByRole("button", { name: DRIVES.NEW_CTA, exact: true }).click();
    const editor = page.getByTestId("drives-drive-editor");

    // The derived id is offered for a MANAGED drive…
    await expect(editor.getByRole("button", { name: DRIVES.HOME_HASH })).toBeEnabled();

    // …and a host share, if this deployment sets no roots, is offered with its
    // REASON rather than offered and refused. Whichever half this deployment
    // is in, the option and its explanation travel together.
    const share = editor.getByRole("button", { name: DRIVES.BACKEND_HOST_PATH });
    if (await share.isDisabled()) {
      await expect(editor.getByText(DRIVES.BACKEND_UNAVAILABLE_DOCKER_ROOTS)).toBeVisible();
    } else {
      await share.click();
      // A share's directories are named by the corporation's own directory,
      // so the derived id cannot name one: disabled here, refused on the API.
      await expect(editor.getByRole("button", { name: DRIVES.HOME_HASH })).toBeDisabled();
      await expect(page.locator("#drive-host-root")).toBeVisible();
      await expect(editor.getByText(DRIVES.HOST_ROOT_HINT)).toBeVisible();
    }
  });

  test("allocating it to a person lands the row and the drive's new count", async ({ page }) => {
    await gotoDrives(page);

    // A person's own row, which beats their group's — and the only tier that
    // may name a directory.
    await page.getByRole("button", { name: PERM.SUBJECT_USER, exact: true }).click();
    await expect(page.getByText(PERM.HINT_USER)).toBeVisible();
    await page.getByRole("textbox", { name: PERM.FIELD_WHO, exact: true }).fill(USER);
    await page.locator("#drive-allocation-drive").click();
    await page.getByRole("option", { name: RENAMED, exact: true }).click();
    // Priority is meaningful only inside the group tier, so a person's row
    // parks it rather than accepting a number that would never be read.
    await expect(page.locator("#drive-allocation-priority")).toBeDisabled();
    await expect(page.getByText(DRIVES.HOME_OVERRIDE_HINT)).toBeVisible();

    // At rest, with a drive to bind, Allocate is the surface's one teal.
    await expect(tealActions(page)).toHaveCount(1);
    const allocate = page.getByRole("button", { name: DRIVES.ADD_CTA, exact: true });
    await expect(allocate).toHaveClass(/bg-primary/);
    await allocate.click();

    const row = allocationsTable(page).getByRole("row", { name: new RegExp(USER) });
    await expect(row.getByText(PERM.SUBJECT_USER, { exact: true })).toBeVisible();
    await expect(row.getByRole("cell", { name: RENAMED, exact: true })).toBeVisible();
    await expect(row.getByText(GOV.PRIORITY_NA)).toBeVisible();
    await expect(row.getByText(DRIVES.OVERRIDES_NONE)).toBeVisible();
    await expect(page.getByText(DRIVES.EMPTY_ALLOC_TITLE)).toHaveCount(0);

    // The drives table above re-reads the same snapshot: one subject now.
    await expect(page.getByText(DRIVES.ALLOCATED_COUNT(1), { exact: true })).toBeVisible();
    await expect(page.getByText(DRIVES.ALLOCATED_NONE)).toHaveCount(0);
  });

  test("delete is REFUSED while allocated: pre-filled from the count, confirm never enables", async ({ page }) => {
    await gotoDrives(page);
    await page.getByRole("button", { name: `${DRIVES.DELETE} ${RENAMED}`, exact: true }).click();

    const dialog = page.getByRole("alertdialog");
    await expect(dialog.getByRole("heading", { name: `Delete "${RENAMED}"?` })).toBeVisible();
    await expect(dialog.getByText(DRIVES.DELETE_RESTRICT_TITLE)).toBeVisible();
    await expect(dialog.getByText(DRIVES.DELETE_RESTRICT_BODY(RENAMED, 1))).toBeVisible();
    // The unallocated consequence would be a false claim here.
    await expect(dialog.getByText(DRIVES.DELETE_CONFIRM(RENAMED))).toHaveCount(0);
    // There is nothing to attempt: the list already knows the count (§2.4).
    await expect(dialog.getByRole("button", { name: DRIVES.DELETE, exact: true })).toBeDisabled();

    await dialog.getByRole("button", { name: PEOPLE.CANCEL, exact: true }).click();
    await expect(drivesTable(page).getByRole("cell", { name: RENAMED, exact: true })).toBeVisible();
  });

  test("the preview asks the SERVER, and its answer names the tier and the object to reclaim", async ({ page }) => {
    await gotoDrives(page);

    await page.locator("#drive-preview-claims").fill(USER);
    await page.getByRole("button", { name: DRIVES.PREVIEW_CTA, exact: true }).click();

    // THE same resolveUserDriveFor the enforcement path takes — there is no
    // client-side precedence at all, so the tier in this sentence could only
    // have come from the server.
    await expect(page.getByText(DRIVES.PREVIEW_RESULT(RENAMED, DRIVES.PREVIEW_TIER_USER))).toBeVisible();
    const answer = page.getByTestId("drives-preview-result");
    await expect(answer).toBeVisible();
    // The object name is what an offboarding command needs, and this is the
    // ONLY surface that prints it — no member-facing string ever does.
    await expect(answer.getByText(DRIVES.PREVIEW_OBJECT_LABEL)).toBeVisible();
    await expect(answer.getByText(DRIVES.PREVIEW_OBJECT_HINT)).toBeVisible();
    await expect(answer.getByText(DRIVES.FIELD_HOME, { exact: true })).toBeVisible();
    await expect(answer.getByText(SIZE)).toBeVisible();
    await expect(answer.getByText(DRIVES.ENFORCEMENT_NONE)).toBeVisible();
    // Nothing was saved, and the screen says so.
    await expect(page.getByText(GOV.PREVIEW_NOT_SAVED)).toBeVisible();
  });

  test("claims nothing matches get the empty answer, not a guess", async ({ page }) => {
    await gotoDrives(page);
    await page.locator("#drive-preview-claims").fill("nobody@corp.example\nno-such-group");
    await page.getByRole("button", { name: DRIVES.PREVIEW_CTA, exact: true }).click();

    // An empty object is "no allocation matched" — the absent-key doctrine.
    await expect(page.getByText(DRIVES.PREVIEW_NONE)).toBeVisible();
    await expect(page.getByTestId("drives-preview-result")).toHaveCount(0);
  });

  test("Remove the allocation, and only then does the drive delete", async ({ page }) => {
    await gotoDrives(page);

    await page.getByRole("button", { name: `${PERM.REMOVE} ${USER}`, exact: true }).click();
    const confirm = page.getByRole("alertdialog");
    // Unallocating is reversible and deletes nothing — the directory stays
    // until the admin reclaims it, and the confirm says so.
    await expect(confirm.getByText(/Nothing on the drive is deleted/)).toBeVisible();
    await confirm.getByRole("button", { name: PERM.REMOVE, exact: true }).click();

    await expect(page.getByText(DRIVES.EMPTY_ALLOC_TITLE)).toBeVisible();
    await expect(page.getByText(DRIVES.ALLOCATED_NONE)).toBeVisible();

    // Now the supported way out: the dialog carries the plain consequence and
    // the confirm is live.
    await page.getByRole("button", { name: `${DRIVES.DELETE} ${RENAMED}`, exact: true }).click();
    const del = page.getByRole("alertdialog");
    await expect(del.getByText(DRIVES.DELETE_RESTRICT_TITLE)).toHaveCount(0);
    const confirmDelete = del.getByRole("button", { name: DRIVES.DELETE, exact: true });
    await expect(confirmDelete).toBeEnabled();
    await confirmDelete.click();

    await expect(page.getByText(DRIVES.EMPTY_TITLE)).toBeVisible();
    await expect(drivesTable(page).getByRole("cell", { name: RENAMED, exact: true })).toHaveCount(0);
  });
});
