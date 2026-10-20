/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Team drives (#1988), behind storage.user_drive.team_drives: the admin editor
// and Drives list, and a member's Files step. The backend that serves a team
// drive is a separate lane, so the drive routes are stateful stubs: what this
// pins is what the console shows and what it sends, not the server's answer.
//
// The security reading: a team drive is read-only unless the admin ticks
// Members can write (and the console sends that bit to the drive AND to every
// allocation); a member is offered only the drives the catalogue lists, and a
// read-only one is locked at Read-only.
import type { Page, Route } from "@playwright/test";
import { test, expect, asRealMember, gotoConsole, navToRoute, walkGuidedToFiles } from "./fixtures";
import { NR_FILES } from "../src/app/components/wardyn/copy/new-run/files";
import { NR_SHARED } from "../src/app/components/wardyn/copy/new-run/shared";
import { NR_START } from "../src/app/components/wardyn/copy/new-run/start";
import { DRIVES, PERM } from "../src/app/lib/user-drives-copy";

const WHEN = "2026-10-20T00:00:00Z";
const REF = { id: "a1111111-1111-4111-8111-111111111111", name: "platform-ref", backend: "docker_volume", home_template: "hash", size_mib: 10240, reclaim: "retain", team: true, single_host: true, created_at: WHEN, updated_at: WHEN, grant_count: 1 };
const SCRATCH = { id: "a2222222-2222-4222-8222-222222222222", name: "team-scratch", backend: "host_path", home_template: "sub", host_root: "/srv/teams", size_mib: 51200, writable: true, reclaim: "retain", team: true, created_at: WHEN, updated_at: WHEN, grant_count: 2 };
const grant = (id: string, drive: string, subject: string, mcw = false) => ({ id, subject_type: "group", subject, drive_id: drive, priority: 0, enabled: true, created_at: WHEN, members_can_write: mcw });
const GRANTS = [grant("b1", REF.id, "platform-team"), grant("b2", SCRATCH.id, "data-team", true), grant("b3", SCRATCH.id, "qa", true)];

type Sent = { puts: { id: string; body: Record<string, unknown> }[]; posts: Record<string, unknown>[]; grants: Record<string, unknown>[]; deletes: string[] };

/** GET /drives, the site config's switch, and the writes, answered the way a server with team drives would. */
async function stubAdmin(page: Page, opts: { on: boolean }): Promise<Sent> {
  const sent: Sent = { puts: [], posts: [], grants: [], deletes: [] };
  await page.route("**/api/v1/workspace-providers", (route) =>
    route.fulfill({ json: { storage: { user_drive: opts.on ? { team_drives: true } : {} } } }),
  );
  await page.route("**/api/v1/drives", (route: Route) => {
    if (route.request().method() === "POST") {
      const body = route.request().postDataJSON() as Record<string, unknown>;
      sent.posts.push(body);
      return route.fulfill({ status: 201, json: { id: "a3333333-3333-4333-8333-333333333333", ...body, created_at: WHEN, updated_at: WHEN } });
    }
    return route.fulfill({ json: { drives: [REF, SCRATCH], grants: GRANTS, grant_total: GRANTS.length, host_roots_configured: true, runner_target: "docker", disabled: false } });
  });
  await page.route("**/api/v1/drives/grants", (route) => {
    sent.grants.push(route.request().postDataJSON() as Record<string, unknown>);
    return route.fulfill({ status: 201, json: GRANTS[0] });
  });
  await page.route("**/api/v1/drives/grants/*", (route) => {
    sent.deletes.push(new URL(route.request().url()).pathname.split("/").pop()!);
    return route.fulfill({ status: 204 });
  });
  await page.route("**/api/v1/drives/a*", (route) => {
    if (route.request().method() !== "PUT") return route.fallback();
    const id = new URL(route.request().url()).pathname.split("/").pop()!;
    const body = route.request().postDataJSON() as Record<string, unknown>;
    sent.puts.push({ id, body });
    return route.fulfill({ json: { id, ...body, created_at: WHEN, updated_at: WHEN } });
  });
  return sent;
}

async function openDrives(page: Page): Promise<void> {
  await gotoConsole(page, "admin");
  await navToRoute(page, "/admin/drives");
  await expect(page.getByRole("heading", { name: DRIVES.TITLE, level: 1 })).toBeVisible();
}

const driveRow = (page: Page, name: string) => page.getByRole("row").filter({ has: page.getByRole("button", { name: `${DRIVES.EDIT} ${name}` }) });
const editor = (page: Page) => page.getByTestId("drives-drive-editor");

test.describe("Admin Drives: team drives", () => {
  test("switch off: the same drives are today's rows and the editor has no team control", async ({ page }) => {
    await stubAdmin(page, { on: false });
    await openDrives(page);
    const row = driveRow(page, REF.name);
    await expect(row).toBeVisible();
    await expect(page.getByText(DRIVES.TEAM_CHIP)).toHaveCount(0);
    await expect(page.getByText(DRIVES.TEAM_RECLAIM_CELL)).toHaveCount(0);
    await row.getByRole("button", { name: `${DRIVES.EDIT} ${REF.name}` }).click();
    await expect(editor(page).getByRole("switch", { name: DRIVES.FIELD_WRITABLE })).toBeVisible();
    await expect(editor(page).getByRole("switch", { name: DRIVES.FIELD_TEAM })).toHaveCount(0);
  });

  test("switch on: team rows carry Team drive, One host only, Kept and the group names; Mode follows the allocations", async ({ page }) => {
    await stubAdmin(page, { on: true });
    await openDrives(page);
    const ref = driveRow(page, REF.name);
    await expect(ref.getByText(DRIVES.TEAM_CHIP, { exact: true })).toBeVisible();
    await expect(ref.getByText(DRIVES.TEAM_ONE_HOST_CHIP)).toBeVisible();
    await expect(ref.getByText(DRIVES.TEAM_RECLAIM_CELL)).toBeVisible();
    await expect(ref.getByRole("cell", { name: "platform-team", exact: true })).toBeVisible();
    await expect(ref.getByText(DRIVES.MODE_RO)).toBeVisible();
    const scratch = driveRow(page, SCRATCH.name);
    await expect(scratch.getByRole("cell", { name: "data-team, qa", exact: true })).toBeVisible();
    await expect(scratch.getByText(DRIVES.MODE_RW)).toBeVisible();
    await expect(scratch.getByText(DRIVES.TEAM_ONE_HOST_CHIP)).toHaveCount(0);
  });

  test("the editor: one write toggle, the Docker notes, no per-person directory; ticking it writes the drive and every allocation", async ({ page }) => {
    const sent = await stubAdmin(page, { on: true });
    await openDrives(page);
    await driveRow(page, REF.name).getByRole("button", { name: `${DRIVES.EDIT} ${REF.name}` }).click();
    const ed = editor(page);
    await expect(ed.getByRole("switch", { name: DRIVES.FIELD_TEAM })).toHaveAttribute("aria-checked", "true");
    await expect(ed.getByText(DRIVES.TEAM_HINT)).toBeVisible();
    await expect(ed.getByText(DRIVES.TEAM_WRITE_HINT)).toBeVisible();
    await expect(ed.getByRole("switch", { name: DRIVES.FIELD_WRITABLE })).toHaveCount(0);
    await expect(ed.getByText(DRIVES.TEAM_ONE_HOST)).toBeVisible();
    await expect(ed.getByText(DRIVES.TEAM_EMPTY_READ_ONLY)).toBeVisible();
    await expect(ed.getByText(DRIVES.TEAM_NO_HOME)).toBeVisible();
    await expect(ed.getByText(DRIVES.TEAM_SIZE_HINT)).toBeVisible();
    await expect(ed.getByText(DRIVES.TEAM_RECLAIM_KEPT)).toBeVisible();
    await expect(ed.getByText(DRIVES.TEAM_ALLOC_HINT)).toBeVisible();
    await expect(ed.getByTestId("drives-team-groups").getByText("platform-team")).toBeVisible();

    // Read-only is where it starts: the toggle is off until the admin turns it on.
    const write = ed.getByRole("switch", { name: DRIVES.FIELD_TEAM_WRITE });
    await expect(write).toHaveAttribute("aria-checked", "false");
    await write.click();
    await expect(ed.getByText(DRIVES.TEAM_EMPTY_READ_ONLY)).toHaveCount(0);
    await ed.getByRole("button", { name: DRIVES.SAVE_CTA }).click();
    await expect.poll(() => sent.grants.length).toBe(1);
    expect(sent.puts).toHaveLength(1);
    expect(sent.puts[0].body).toMatchObject({ team: true, writable: true, reclaim: "retain" });
    expect(sent.grants[0]).toMatchObject({ subject_type: "group", subject: "platform-team", drive_id: REF.id, additional: true, members_can_write: true });
  });

  test("a new team drive is saved read-only with the group the admin allocated", async ({ page }) => {
    const sent = await stubAdmin(page, { on: true });
    await openDrives(page);
    await page.getByRole("button", { name: DRIVES.NEW_CTA, exact: true }).click();
    const ed = editor(page);
    await ed.getByLabel(DRIVES.FIELD_NAME).fill("shared-notes");
    await ed.getByRole("switch", { name: DRIVES.FIELD_TEAM }).click();
    await expect(ed.getByRole("switch", { name: DRIVES.FIELD_TEAM_WRITE })).toHaveAttribute("aria-checked", "false");
    await ed.getByRole("textbox", { name: PERM.SUBJECT_GROUP }).fill("data-team");
    await ed.getByRole("button", { name: DRIVES.ADD_CTA }).click();
    await expect(ed.getByTestId("drives-team-groups").getByText("data-team")).toBeVisible();
    await ed.getByRole("button", { name: DRIVES.SAVE_CTA }).click();
    await expect.poll(() => sent.grants.length).toBe(1);
    expect(sent.posts[0]).toMatchObject({ name: "shared-notes", team: true, writable: false, reclaim: "retain" });
    expect(sent.grants[0]).toMatchObject({ subject_type: "group", subject: "data-team", additional: true, members_can_write: false });
  });

  test("removing a group chip unallocates it", async ({ page }) => {
    const sent = await stubAdmin(page, { on: true });
    await openDrives(page);
    await driveRow(page, SCRATCH.name).getByRole("button", { name: `${DRIVES.EDIT} ${SCRATCH.name}` }).click();
    const ed = editor(page);
    await ed.getByRole("button", { name: `${PERM.REMOVE} qa` }).click();
    await ed.getByRole("button", { name: DRIVES.SAVE_CTA }).click();
    await expect.poll(() => sent.deletes).toEqual(["b3"]);
    expect(sent.grants).toHaveLength(0);
  });
});

// ---- a member's Files step ----

const RO = { id: "c1111111-1111-4111-8111-111111111111", name: "platform-ref", section: "user", source: "team", backend: "docker_volume", target: "/home/agent/shared/platform-ref", writable: false, enforcement: "none", legacy: false, owned: false, team: true, shared_with: ["platform-team"] };
const RW = { id: "c2222222-2222-4222-8222-222222222222", name: "team-scratch", section: "shared", source: "team", backend: "host_path", target: "/home/agent/shared/team-scratch", writable: true, enforcement: "none", legacy: false, owned: false, team: true, shared_with: ["data-team", "qa"] };
const POOL = { id: "f2222222-2222-4222-8222-222222222222", name: "General", hosting_type: "remote_provided" };

/** The member's catalogue, and a preflight that echoes the drives with the access they were given. */
async function stubMember(page: Page): Promise<void> {
  await page.route("**/api/v1/me/drives**", (route) => route.fulfill({ json: { drives: [RO, RW], pool_checked: true } }));
  await page.route("**/api/v1/runs/preflight", (route) => {
    const body = route.request().postDataJSON() as { drives?: { drive_id: string; read_only?: boolean }[] };
    return route.fulfill({
      json: {
        setup_items: [],
        enforced_confinement_class: "CC2",
        runner_pool: { ...POOL, revision: 1, selection: "organisation_default" },
        runner_pools: [{ ...POOL, availability: "available", limits: [] }],
        main_drive: { applied_mib: 10240, basis: "default", soft: false },
        attachments: (body.drives ?? []).map((d, i) => {
          const drive = [RO, RW].find((x) => x.id === d.drive_id)!;
          return { kind: "drive", drive_id: d.drive_id, name: drive.name, target: drive.target, read_only: d.read_only === true || !drive.writable, starts_here: i === 0 };
        }),
      },
    });
  });
}

test.describe("New Run, Files: team drives (member)", () => {
  test("a read-only team drive is added locked, says who shares it, and the mount map says read-only; a writable one warns the group", async ({ page }) => {
    await asRealMember(page);
    await stubMember(page);
    await gotoConsole(page);
    await navToRoute(page, "/runs/new");
    await expect(page.getByRole("heading", { name: NR_START.TITLE })).toBeVisible();
    await walkGuidedToFiles(page, { shape: "agent_task" });

    await page.getByRole("button", { name: NR_FILES.ADD_ARIA }).click();
    const dialog = page.getByRole("dialog");
    await dialog.getByRole("button", { name: new RegExp(`^${NR_FILES.KIND_SHARED_DRIVE}`) }).click();
    await expect(dialog.getByRole("heading", { name: NR_FILES.ADD_SHARED_DRIVE_TITLE })).toBeVisible();

    const ro = dialog.getByRole("radio", { name: new RegExp(`^${RO.name}`) }).locator("xpath=ancestor::label");
    await expect(ro.getByText(NR_FILES.SOURCE_TEAM, { exact: true })).toBeVisible();
    await expect(ro.getByText(NR_FILES.ACCESS_READ, { exact: true })).toBeVisible();
    await expect(ro.getByText(NR_FILES.SHARED_WITH("platform-team"))).toBeVisible();
    await expect(ro.getByText(NR_FILES.TEAM_OTHERS_SEE)).toHaveCount(0);
    const rw = dialog.getByRole("radio", { name: new RegExp(`^${RW.name}`) }).locator("xpath=ancestor::label");
    await expect(rw.getByText(NR_FILES.ACCESS_EDIT, { exact: true })).toBeVisible();
    await expect(rw.getByText(NR_FILES.SHARED_WITH("data-team, qa"))).toBeVisible();
    await expect(rw.getByText(NR_FILES.TEAM_OTHERS_SEE)).toBeVisible();

    await dialog.getByRole("radio", { name: new RegExp(`^${RO.name}`) }).check();
    await dialog.getByRole("button", { name: NR_SHARED.CONTINUE, exact: true }).click();
    // Locked: no way to choose Can edit.
    await expect(dialog.getByRole("button", { name: NR_FILES.LOCKED_ARIA })).toBeVisible();
    await expect(dialog.locator("select#nr-mount-access")).toHaveCount(0);
    await dialog.getByRole("button", { name: NR_FILES.ADD_CONFIRM, exact: true }).click();

    const card = page.getByTestId("nr-attach-card").filter({ hasText: RO.name });
    await expect(card).toHaveCount(1);
    await expect(card.getByText(NR_FILES.SHARED_WITH("platform-team"))).toBeVisible();
    await expect(card.getByText(NR_FILES.ACCESS_READ, { exact: true })).toBeVisible();
    await expect(card.getByText(NR_FILES.ACCESS_EDIT, { exact: true })).toHaveCount(0);
    const mounted = page.getByRole("region", { name: NR_FILES.MOUNT_TITLE }).getByRole("listitem").filter({ hasText: NR_FILES.MOUNT_ITEM(RO.name) });
    await expect(mounted).toContainText(NR_FILES.MOUNT_READ_ONLY);
  });
});
