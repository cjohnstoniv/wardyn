/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New Run, Files: person-created drives. Against the real backend, the switch is
// off by default and, once an admin switches it on, the hermetic `none` runner
// cannot delete a drive, so the server closes the door with its reason. The
// create and delete flow itself needs a substrate that can delete, which this
// backend has none of, so it runs against stateful stubs of the three drive routes.
import type { APIRequestContext, Locator, Page } from "@playwright/test";
import { test, expect, ADMIN_TOKEN, asRealMember, gotoConsole, navToRoute, walkGuidedToFiles } from "./fixtures";
import { NR_FILES } from "../src/app/components/wardyn/copy/new-run/files";
import { NR_SHARED } from "../src/app/components/wardyn/copy/new-run/shared";
import { NR_START } from "../src/app/components/wardyn/copy/new-run/start";
import { DRIVE_REFUSAL } from "../src/app/lib/drive-refusals";

const POOL = { id: "f2222222-2222-4222-8222-222222222222", name: "General", hosting_type: "remote_provided" };
const admin = { Authorization: `Bearer ${ADMIN_TOKEN}` };

type Providers = { storage?: { user_drive?: { member_create?: { enabled?: boolean; max_per_person?: number } } } };

/** The admin route that switches person-created drives on (storage.user_drive.member_create) or back off. */
async function setMemberDrives(request: APIRequestContext, on: boolean): Promise<void> {
  const got = await request.get("/api/v1/workspace-providers", { headers: admin });
  expect(got.status()).toBe(200);
  const doc = (await got.json()) as Providers;
  const next: Providers = { ...doc, storage: { ...doc.storage, user_drive: { ...doc.storage?.user_drive, member_create: on ? { enabled: true, max_per_person: 3 } : undefined } } };
  const put = await request.put("/api/v1/workspace-providers", { headers: { ...admin, "If-Match": got.headers()["etag"] ?? "" }, data: next });
  expect(put.status(), await put.text()).toBe(200);
}

async function openUserDrives(page: Page): Promise<Locator> {
  await page.getByRole("button", { name: NR_FILES.ADD_ARIA }).click();
  const dialog = page.getByRole("dialog");
  await dialog.getByRole("button", { name: new RegExp(`^${NR_FILES.KIND_USER_DRIVE}`) }).click();
  await expect(dialog.getByRole("heading", { name: NR_FILES.ADD_USER_DRIVE_TITLE })).toBeVisible();
  return dialog;
}

async function toFiles(page: Page): Promise<void> {
  await gotoConsole(page);
  await navToRoute(page, "/runs/new");
  await expect(page.getByRole("heading", { name: NR_START.TITLE })).toBeVisible();
  await walkGuidedToFiles(page, { shape: "agent_task" });
}

test.describe("New Run, Files: person-created drives against the real backend", () => {
  test.afterEach(async ({ request }) => setMemberDrives(request, false));

  test("off by default there is no create control; switched on, the control is closed with the server's reason", async ({ page, request }) => {
    await asRealMember(page);
    await toFiles(page);
    let dialog = await openUserDrives(page);
    await expect(dialog.getByRole("button", { name: NR_FILES.CREATE_USER_DRIVE })).toHaveCount(0);
    await page.keyboard.press("Escape");

    await setMemberDrives(request, true);
    await page.reload();
    await toFiles(page);
    dialog = await openUserDrives(page);
    const control = dialog.getByRole("button", { name: NR_FILES.CREATE_USER_DRIVE });
    await expect(control).toBeDisabled();
    // The hermetic runner cannot delete a drive, and a drive that cannot be deleted is never created.
    await expect(dialog.getByText(NR_FILES.CREATE_NO_DELETE)).toBeVisible();
  });
});

const DRIVE_ID = "f3333333-3333-4333-8333-333333333333";
const NAME = "Case notes";

/** GET/POST/DELETE /me/drives as a deployment that can delete would answer them, and a preflight that echoes the drives. */
async function stubDrives(page: Page, opts: { refuseFirstCreate?: boolean } = {}): Promise<{ posts: { name: string }[]; deletes: string[] }> {
  const seen = { posts: [] as { name: string }[], deletes: [] as string[] };
  let drives: object[] = [];
  let refuse = opts.refuseFirstCreate === true;
  const row = { id: DRIVE_ID, name: NAME, section: "user", source: "personal", backend: "docker_volume", target: "/home/agent/drives/case-notes", writable: true, enforcement: "none", legacy: false, owned: true };
  await page.route("**/api/v1/me/drives**", async (route) => {
    const req = route.request();
    if (req.method() === "POST") {
      const body = req.postDataJSON() as { name: string };
      seen.posts.push(body);
      if (refuse) {
        refuse = false;
        return route.fulfill({ status: 422, json: { error: DRIVE_REFUSAL.MEMBER_NAME_TAKEN(body.name), reason: "member_drive_name_taken" } });
      }
      drives = [{ ...row, name: body.name }];
      return route.fulfill({ status: 201, json: { ...row, name: body.name } });
    }
    if (req.method() === "DELETE") {
      seen.deletes.push(new URL(req.url()).pathname.split("/").pop()!);
      drives = [];
      return route.fulfill({ status: 204 });
    }
    return route.fulfill({ json: { drives, pool_checked: true, create: { allowed: true, count: drives.length, limit: 3 } } });
  });
  await page.route("**/api/v1/runs/preflight", async (route) => {
    const body = route.request().postDataJSON() as { drives?: { drive_id: string }[] };
    return route.fulfill({
      json: {
        setup_items: [],
        enforced_confinement_class: "CC2",
        runner_pool: { ...POOL, revision: 1, selection: "organisation_default" },
        runner_pools: [{ ...POOL, availability: "available", limits: [] }],
        main_drive: { applied_mib: 10240, basis: "default", soft: false },
        attachments: (body.drives ?? []).map((d) => ({ kind: "drive", drive_id: d.drive_id, name: NAME, target: row.target, read_only: false, starts_here: true })),
      },
    });
  });
  return seen;
}

test.describe("New Run, Files: create and delete a person's drive", () => {
  test("a refused name keeps the dialog open; the retry creates the drive, chooses it and adds it", async ({ page }) => {
    const seen = await stubDrives(page, { refuseFirstCreate: true });
    await toFiles(page);
    const dialog = await openUserDrives(page);
    const link = dialog.getByRole("button", { name: NR_FILES.CREATE_USER_DRIVE });
    await link.click();
    const create = page.getByRole("dialog", { name: NR_FILES.CREATE_TITLE });
    await expect(create.getByLabel(NR_FILES.DRIVE_NAME)).toBeFocused();
    await create.getByLabel(NR_FILES.DRIVE_NAME).fill(NAME);
    await create.getByRole("button", { name: NR_FILES.CREATE_CONFIRM }).click();
    await expect(create.getByRole("alert")).toHaveText(DRIVE_REFUSAL.MEMBER_NAME_TAKEN(NAME));

    await create.getByRole("button", { name: NR_FILES.CREATE_CONFIRM }).click();
    await expect(create).toHaveCount(0);
    expect(seen.posts).toEqual([{ name: NAME }, { name: NAME }]);
    await expect(link).toBeFocused();
    // Chosen, and nothing is launched: the person adds it like any other drive.
    await expect(dialog.getByRole("radio", { name: new RegExp(`^${NAME}`) })).toBeChecked();
    await dialog.getByRole("button", { name: NR_SHARED.CONTINUE, exact: true }).click();
    await dialog.getByRole("button", { name: NR_FILES.ADD_CONFIRM, exact: true }).click();
    await expect(page.getByTestId("nr-attach-card").filter({ hasText: NAME })).toHaveCount(1);
  });

  test("Escape closes only the create dialog", async ({ page }) => {
    await stubDrives(page);
    await toFiles(page);
    const dialog = await openUserDrives(page);
    await dialog.getByRole("button", { name: NR_FILES.CREATE_USER_DRIVE }).click();
    await expect(page.getByRole("dialog", { includeHidden: true })).toHaveCount(2);
    await page.keyboard.press("Escape");
    await expect(page.getByRole("dialog", { includeHidden: true })).toHaveCount(1);
    await expect(dialog.getByRole("button", { name: NR_FILES.CREATE_USER_DRIVE })).toBeFocused();
  });

  test("deleting a drive asks first, then takes it out of the list and off the run", async ({ page }) => {
    const seen = await stubDrives(page);
    await toFiles(page);
    let dialog = await openUserDrives(page);
    await dialog.getByRole("button", { name: NR_FILES.CREATE_USER_DRIVE }).click();
    await page.getByLabel(NR_FILES.DRIVE_NAME).fill(NAME);
    await page.getByRole("button", { name: NR_FILES.CREATE_CONFIRM }).click();
    await dialog.getByRole("button", { name: NR_SHARED.CONTINUE, exact: true }).click();
    await dialog.getByRole("button", { name: NR_FILES.ADD_CONFIRM, exact: true }).click();
    const card = page.getByTestId("nr-attach-card").filter({ hasText: NAME });
    await expect(card).toHaveCount(1);

    dialog = await openUserDrives(page);
    await dialog.getByTestId("nr-own-drives").getByRole("button", { name: NR_FILES.DELETE_DRIVE }).click();
    const confirm = page.getByRole("dialog", { name: NR_FILES.DELETE_CONFIRM(NAME) });
    await expect(confirm.getByRole("button", { name: NR_SHARED.CANCEL })).toBeFocused();
    await confirm.getByRole("button", { name: NR_FILES.DELETE_DRIVE }).click();
    await expect(confirm).toHaveCount(0);
    expect(seen.deletes).toEqual([DRIVE_ID]);
    await expect(dialog.getByTestId("nr-own-drives")).toHaveCount(0);
    await expect(dialog.getByRole("radio", { name: new RegExp(`^${NAME}`) })).toHaveCount(0);
    await dialog.getByRole("button", { name: NR_SHARED.CANCEL, exact: true }).click();
    await expect(card).toHaveCount(0);
  });
});
