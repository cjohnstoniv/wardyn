/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Where a drive mounts from (mock M10, #113 + #112) on the admin Drives screen.
// The drive routes are stateful stubs: what this pins is what the console shows
// and sends, the Mounts from column printed as the server says it, the label
// under Backend, and a share's "Also reachable from" section writing one second
// place on a person's own runner.
import type { Page, Route } from "@playwright/test";
import { test, expect, gotoConsole, navToRoute } from "./fixtures";
import { DRIVES, PEOPLE } from "../src/app/lib/user-drives-copy";

// A display-only timestamp, an hour ago.
const WHEN = new Date(Date.now() - 3_600_000).toISOString();
const MBP = "c1111111-1111-4111-8111-111111111111";
const SHARE = {
  id: "a1111111-1111-4111-8111-111111111111", name: "team-share", backend: "host_path", home_template: "email_local", host_root: "/srv/shares/team",
  size_mib: 51200, writable: true, reclaim: "retain", created_at: WHEN, updated_at: WHEN, grant_count: 1,
  placement_eligible: true, mounts_from: "Organisation runners",
};
const SCRATCH = {
  id: "a2222222-2222-4222-8222-222222222222", name: "scratch", backend: "docker_volume", home_template: "hash",
  size_mib: 10240, writable: true, reclaim: "retain", created_at: WHEN, updated_at: WHEN, grant_count: 2,
  placement_eligible: false, mounts_from: "Organisation runners",
};

async function stub(page: Page): Promise<{ id: string; body: Record<string, unknown> }[]> {
  const puts: { id: string; body: Record<string, unknown> }[] = [];
  await page.route("**/api/v1/workspace-providers", (route) => route.fulfill({ json: { storage: { user_drive: {} } } }));
  await page.route("**/api/v1/drives", (route: Route) =>
    route.fulfill({ json: { drives: [SHARE, SCRATCH], grants: [], grant_total: 0, host_roots_configured: true, runner_target: "docker", disabled: false } }),
  );
  await page.route("**/api/v1/runners?*", (route) =>
    route.fulfill({ json: [{ id: MBP, name: "mbp-dev", state: "claimed", owner_kind: "person", owner: "ana@example.com", kind: "self_hosted" }] }),
  );
  await page.route("**/api/v1/drives/a*", (route) => {
    if (route.request().method() !== "PUT") return route.fallback();
    const id = new URL(route.request().url()).pathname.split("/").pop()!;
    const body = route.request().postDataJSON() as Record<string, unknown>;
    puts.push({ id, body });
    return route.fulfill({ json: { id, ...body, created_at: WHEN, updated_at: WHEN } });
  });
  return puts;
}

const editor = (page: Page) => page.getByTestId("drives-drive-editor");

test.describe("Admin Drives: where a drive mounts from", () => {
  test("the Mounts from column, the label under Backend, and a second place on a person's own runner", async ({ page }) => {
    const puts = await stub(page);
    await gotoConsole(page, "admin");
    await navToRoute(page, "/admin/drives");
    await expect(page.getByRole("heading", { name: DRIVES.TITLE, level: 1 })).toBeVisible();
    await expect(page.getByRole("columnheader", { name: DRIVES.COL_MOUNTS_FROM })).toBeVisible();
    await expect(page.getByTestId("drives-mounts-from")).toHaveText(["Organisation runners", "Organisation runners"]);

    await page.getByRole("button", { name: `${DRIVES.EDIT} ${SCRATCH.name}` }).click();
    await expect(editor(page).getByText(DRIVES.LABEL_ONE_PLACE)).toBeVisible();
    await expect(editor(page).getByText(DRIVES.SECOND_PLACE_SHARE_ONLY)).toBeVisible();
    await editor(page).getByRole("button", { name: PEOPLE.CANCEL }).click();

    await page.getByRole("button", { name: `${DRIVES.EDIT} ${SHARE.name}` }).click();
    await expect(editor(page).getByText(DRIVES.LABEL_TWO_PLACES)).toBeVisible();
    await expect(editor(page).getByText(DRIVES.LABEL_TWO_PLACES_BODY)).toBeVisible();
    await editor(page).getByRole("button", { name: DRIVES.SECOND_PLACE_ADD }).click();
    const place = editor(page).getByTestId("drive-second-place");
    await expect(place.getByRole("heading", { name: DRIVES.SECOND_PLACE_TITLE })).toBeVisible();
    await expect(place.getByText(DRIVES.WHERE_OWN_RUNNER, { exact: true })).toBeVisible();
    await expect(place.getByText(DRIVES.REACH_NOTE)).toBeVisible();
    await place.getByLabel(DRIVES.FIELD_RUNNER, { exact: true }).selectOption({ label: DRIVES.RUNNER_OPTION("mbp-dev") });
    await place.getByLabel(DRIVES.FIELD_ALT_HOST_ROOT).fill("/Volumes/team-share");
    await editor(page).getByRole("button", { name: DRIVES.SAVE_CTA }).click();
    await expect.poll(() => puts.length).toBe(1);
    expect(puts[0]).toMatchObject({ id: SHARE.id, body: { alt_backend: "host_path", alt_host_root: "/Volumes/team-share", alt_runner_id: MBP } });
  });
});
