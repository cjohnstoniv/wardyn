/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New Run, step 4 Files (public half): the main drive's size and the cap that
// clamps it, the branch picker and its text fallback, Starts here, the mount
// map and the live summary, "No extra files", and the person's own drive from
// /me.user_drive. The plural drive rows and member create belong to the
// private drives spec; nothing here reads the drives list route.
//
// The seeded backend runs no runner and holds no pool or repository, so the
// workspace list, the branch list and the preflight (whose answer carries the
// pool's limits and the mount paths) are stubbed. The preflight is computed
// from the body it was sent, so the mount map follows what the person chose.
import type { Page } from "@playwright/test";
import { test, expect, gotoConsole, navToRoute } from "./fixtures";
import { NR_FILES } from "../src/app/components/wardyn/copy/new-run/files";
import { NR_SHAPE } from "../src/app/components/wardyn/copy/new-run/shape";
import { NR_SHARED } from "../src/app/components/wardyn/copy/new-run/shared";
import { NR_START } from "../src/app/components/wardyn/copy/new-run/start";
import { DRIVE_MEMBER } from "../src/app/lib/user-drives-copy";

const WS_ID = "f1111111-1111-4111-8111-111111111111";
const POOL = { id: "f2222222-2222-4222-8222-222222222222", name: "General", hosting_type: "remote_provided" };
const REPOS = [
  { repo: "https://github.com/acme/web.git", name: "web", target: "/home/agent/work/web" },
  { repo: "https://github.com/acme/docs.git", name: "docs", target: "/home/agent/work/docs" },
];
const WORKSPACE = {
  id: WS_ID,
  name: "Acme",
  kind: "",
  source: "",
  available_to_you: true,
  sources: REPOS.map((r) => ({ type: "repo", source: r.repo, ref: "main", clone_target: r.target })),
};
const DRIVE = { name: "Scratch", backend: "docker_volume", size_mib: 5120, writable: true, enforcement: "filesystem" };
const CAP_MIB = 20480;
const DEFAULT_MIB = 10240;

type Body = { workspaces?: { workspace_id: string; repos?: { repo: string; ref?: string }[] }[]; drive?: { enabled: boolean; read_only?: boolean }; start_folder?: { attachment: string; repo?: string }; resources?: { disk_mib?: number }; no_repositories_or_drives?: boolean };

const unlimited = { default: { value: 0, unlimited: true }, cap: { value: 0, unlimited: true } };
const limitsFor = (run_type: string, capSource: string) => ({
  run_type,
  cpu_millis: unlimited,
  memory_mib: unlimited,
  disk_mib: { default: { value: DEFAULT_MIB }, cap: { value: CAP_MIB }, default_source: "pool", cap_source: capSource },
  lifetime_sec: unlimited,
  lifetime_span_sec: unlimited,
  lifetime_lease_sec: unlimited,
  no_end_allowed: true,
  concurrency: [],
});

/** What the server would answer for `body`: the pool's limits, the main drive's facts and the mounts it chose. */
function preflightFor(body: Body, capSource: string) {
  const attachments: object[] = [];
  for (const w of body.workspaces ?? []) {
    for (const r of w.repos ?? REPOS.map(({ repo }) => ({ repo }))) {
      const row = REPOS.find((x) => x.repo === r.repo)!;
      attachments.push({
        kind: "repository", workspace_id: w.workspace_id, repo: r.repo, name: row.name, target: row.target, read_only: false,
        starts_here: body.start_folder?.attachment === w.workspace_id && body.start_folder.repo === r.repo,
      });
    }
  }
  if (body.drive?.enabled) {
    attachments.push({ kind: "drive", name: DRIVE.name, target: "/home/agent/drive", read_only: body.drive.read_only === true, starts_here: body.start_folder?.attachment === "drive" });
  }
  return {
    setup_items: [],
    enforced_confinement_class: "CC2",
    runner_pool: { ...POOL, revision: 1, selection: "organisation_default" },
    runner_pools: [{ ...POOL, availability: "available", limits: [limitsFor("interactive", capSource), limitsFor("background", capSource)] }],
    main_drive: { applied_mib: body.resources?.disk_mib ?? DEFAULT_MIB, basis: "default", soft: false, facts: { enforcement: "filesystem", basis: "substrate", keep: { supported: false }, drive_mounts: true } },
    attachments,
  };
}

interface Opts {
  capSource?: string;
  /** A branch listing; absent makes the list unavailable, so the text field shows. */
  branches?: string[];
  userDrive?: object | null;
  deniedBy?: string;
}

/** Stubs the reads the step makes and returns the preflight bodies it sent. */
async function stub(page: Page, opts: Opts = {}): Promise<Body[]> {
  const sent: Body[] = [];
  await page.route("**/api/v1/workspaces*", (route) => route.fulfill({ json: [WORKSPACE] }));
  await page.route(`**/api/v1/workspaces/${WS_ID}/branches*`, (route) =>
    route.fulfill({
      json: opts.branches
        ? { listing: "listed", branches: opts.branches, default: opts.branches[0], truncated: false }
        : { listing: "unavailable", branches: [], truncated: false, reason: "no_credential" },
    }),
  );
  await page.route("**/api/v1/runner-pools*", (route) =>
    route.fulfill({
      json: {
        pools: [{ ...POOL, availability: "available", run_types: ["background", "interactive"], run_types_source: "pool", barrier_options: [{ class: "CC1", available: true, default: true, default_source: "server" }] }],
        default: { pool_id: POOL.id, selection: "organisation_default" },
      },
    }),
  );
  await page.route("**/api/v1/runs/preflight", (route) => {
    const body = route.request().postDataJSON() as Body;
    sent.push(body);
    return route.fulfill({ json: preflightFor(body, opts.capSource ?? "pool") });
  });
  if (opts.userDrive !== undefined || opts.deniedBy) {
    await page.route("**/api/v1/me", async (route) => {
      const response = await route.fetch();
      const json = await response.json();
      json.user_drive = opts.userDrive ?? null;
      json.user_drive_denied_by_profile = opts.deniedBy ?? "";
      await route.fulfill({ response, json });
    });
  }
  return sent;
}

const press = (page: Page, name: string) => page.getByRole("button", { name, exact: true }).click();
const next = (page: Page, step: string) => press(page, NR_SHARED.CONTINUE_TO(step));
const sizeInput = (page: Page) => page.locator("#nr-main-drive-size");
const include = (page: Page, name: string) => page.getByRole("checkbox", { name: NR_FILES.INCLUDE_ARIA(name) });
const startsHere = (page: Page, name: string) => page.getByRole("radio", { name: `${name} ${NR_FILES.STARTS_HERE}` });
const last = (sent: Body[]) => sent.at(-1)!;

/** Guided to the Files step for a workload. An Application needs its command on the Run step. */
async function toFiles(page: Page, workload: "environment" | "application" = "environment") {
  await gotoConsole(page);
  await navToRoute(page, "/runs/new");
  await press(page, NR_START.GUIDED_FOOT);
  await page.getByRole("radio", { name: workload === "environment" ? NR_SHAPE.ENV_TITLE : NR_SHAPE.APP_TITLE }).click();
  await next(page, NR_SHARED.STEP_RUN);
  if (workload === "application") await page.locator("#nr-command").fill("npm test");
  await next(page, NR_SHARED.STEP_HARNESSES);
  await next(page, NR_SHARED.STEP_FILES);
  await expect(page.getByRole("heading", { name: NR_FILES.TITLE })).toBeVisible();
}

test.describe("New Run, Files: the main drive", () => {
  const clamps: [string, (cap: string) => string][] = [
    ["pool", (cap) => NR_FILES.CLAMP_POOL(cap, POOL.name)],
    ["governance", (cap) => NR_FILES.CLAMP_ORG(cap)],
    ["deployment", (cap) => NR_FILES.CLAMP_SERVER(cap)],
  ];

  for (const [source, sentence] of clamps) {
    test(`a size over the cap is clamped to it and says it was set by the ${source}`, async ({ page }) => {
      const sent = await stub(page, { capSource: source });
      await toFiles(page);

      // The pool's default fills the field and its cap is stated; an untouched size is not a request.
      await expect(sizeInput(page)).toHaveValue("10");
      await expect(page.getByText(NR_FILES.UP_TO("20"))).toBeVisible();
      await expect(page.getByText(NR_FILES.ENFORCED, { exact: true })).toBeVisible();
      await expect.poll(() => sent.length).toBeGreaterThan(0);
      expect(last(sent)).not.toHaveProperty("resources");

      await sizeInput(page).fill("50");
      await sizeInput(page).blur();
      await expect(sizeInput(page)).toHaveValue("20");
      await expect(page.getByText(sentence("20"))).toBeVisible();
      await expect.poll(() => last(sent).resources?.disk_mib).toBe(CAP_MIB);
    });
  }

  test("an emptied size holds Continue and asks for one", async ({ page }) => {
    await stub(page);
    await toFiles(page);
    await sizeInput(page).fill("");
    await sizeInput(page).blur();
    await expect(page.getByText(NR_FILES.SIZE_ERROR)).toBeVisible();
    await next(page, NR_SHARED.STEP_TOOLS);
    await expect(sizeInput(page)).toBeFocused();
    await expect(page).toHaveURL(/step=files/);
  });
});

test.describe("New Run, Files: repositories", () => {
  test("the branch picker lists the repository's branches and sends a choice", async ({ page }) => {
    const sent = await stub(page, { branches: ["main", "release", "dev"] });
    await toFiles(page);
    await include(page, "web").check();
    const branch = page.getByRole("combobox", { name: NR_FILES.BRANCH_ARIA("web") });
    await expect(branch.locator("option")).toHaveText(["main", "release", "dev"]);
    await branch.selectOption("release");
    await expect.poll(() => last(sent).workspaces?.[0].repos).toEqual([{ repo: REPOS[0].repo, ref: "release" }]);
  });

  test("a branch list that cannot be read falls back to a text field that says it is not checked", async ({ page }) => {
    const sent = await stub(page);
    await toFiles(page);
    await include(page, "web").check();
    const field = page.getByRole("textbox", { name: NR_FILES.BRANCH_ARIA("web") });
    await expect(field).toHaveAttribute("placeholder", NR_FILES.BRANCH_PLACEHOLDER);
    await expect(page.getByText(NR_FILES.BRANCH_FALLBACK)).toBeVisible();
    await expect(page.getByText(NR_FILES.REF_NOT_CHECKED)).toHaveCount(0);

    await field.fill("feature/x");
    await expect(page.getByText(NR_FILES.REF_NOT_CHECKED)).toBeVisible();
    await expect.poll(() => last(sent).workspaces?.[0].repos).toEqual([{ repo: REPOS[0].repo, ref: "feature/x" }]);
  });

  test("Starts here never attaches anything: an item that is out cannot be chosen, and a held Continue points at the choice", async ({ page }) => {
    const sent = await stub(page, { branches: ["main"] });
    await toFiles(page);
    // Nothing is included: every Starts here is off and nothing was attached by looking at it.
    await expect(startsHere(page, "web")).toBeDisabled();
    await expect(startsHere(page, "docs")).toBeDisabled();

    await include(page, "web").check();
    await expect(startsHere(page, "web")).toBeEnabled();
    await expect(startsHere(page, "web")).not.toBeChecked();
    await expect(startsHere(page, "docs")).toBeDisabled();
    await expect.poll(() => sent.length).toBeGreaterThan(0);
    expect(last(sent)).not.toHaveProperty("start_folder");
    expect(last(sent).workspaces?.[0].repos).toEqual([{ repo: REPOS[0].repo }]);

    // Included but no starting folder: Continue stays and focuses the radio.
    await next(page, NR_SHARED.STEP_TOOLS);
    await expect(startsHere(page, "web")).toBeFocused();
    await expect(page).toHaveURL(/step=files/);

    await startsHere(page, "web").check();
    await expect.poll(() => last(sent).start_folder).toEqual({ kind: "attachment", attachment: WS_ID, repo: REPOS[0].repo });
    expect(last(sent).workspaces).toHaveLength(1);
  });

  test("the mount map and the summary follow what is included and where it starts", async ({ page }) => {
    await stub(page, { branches: ["main"] });
    await toFiles(page);
    await expect(page.getByRole("status").filter({ hasText: NR_FILES.CHOOSE_FILES })).toBeVisible();

    await include(page, "web").check();
    await startsHere(page, "web").check();
    const map = page.getByRole("region", { name: NR_FILES.MOUNT_TITLE });
    await expect(map).toContainText(NR_FILES.MOUNT_MAIN("10"));
    await expect(map).toContainText(`└ ${REPOS[0].target}`);
    await expect(map).toContainText(`${NR_FILES.MOUNT_ITEM("web")} ${NR_FILES.MOUNT_STARTS}`);

    const summary = [NR_FILES.SUMMARY_MAIN("10"), NR_FILES.SUMMARY_STARTS("web"), NR_FILES.SUMMARY_DELETED].reduce((all, p) => NR_SHARED.JOIN(all, p));
    await expect(page.getByRole("status").filter({ hasText: summary })).toBeVisible();
  });

  test("No extra files is a choice: only the main drive, and a repository chosen later replaces it", async ({ page }) => {
    const sent = await stub(page, { branches: ["main"] });
    await toFiles(page);
    await page.locator("#nr-files-choice").check();
    await expect.poll(() => last(sent).no_repositories_or_drives).toBe(true);
    const nothing = NR_FILES.SUMMARY_NO_EXTRA("10", NR_FILES.SUMMARY_DELETED);
    await expect(page.getByRole("status").filter({ hasText: nothing })).toBeVisible();
    await expect(page.getByRole("region", { name: NR_FILES.MOUNT_TITLE }).getByText(/←/)).toHaveCount(0);

    await include(page, "docs").check();
    await expect(page.locator("#nr-files-choice")).not.toBeChecked();
    await expect.poll(() => last(sent).no_repositories_or_drives).toBeUndefined();
    await expect.poll(() => last(sent).workspaces?.[0].repos).toEqual([{ repo: REPOS[1].repo }]);
  });
});

test.describe("New Run, Files: the person's drive (public)", () => {
  test("a drive is off by default; mounting it, read-only and starting there reach the wire and the map", async ({ page }) => {
    const sent = await stub(page, { userDrive: DRIVE });
    await toFiles(page, "application");
    await expect(page.getByRole("heading", { name: NR_FILES.USER_DRIVES_TITLE })).toBeVisible();
    const mount = page.getByRole("checkbox", { name: DRIVE_MEMBER.NR_CHECKBOX });
    await expect(mount).not.toBeChecked();
    await expect(page.getByRole("checkbox", { name: DRIVE_MEMBER.NR_READONLY_TOGGLE })).toHaveCount(0);

    await mount.check();
    await expect(page.getByTestId("nr-drive")).toContainText(DRIVE_MEMBER.NR_RW_NOTE);
    await page.getByRole("checkbox", { name: DRIVE_MEMBER.NR_READONLY_TOGGLE }).check();
    await expect(page.getByTestId("nr-drive")).toContainText(DRIVE_MEMBER.NR_RO_NOTE);
    await startsHere(page, DRIVE_MEMBER.NR_CHECKBOX).check();
    await expect.poll(() => last(sent).drive).toMatchObject({ enabled: true, read_only: true });
    expect(last(sent).start_folder).toEqual({ kind: "attachment", attachment: "drive" });

    const map = page.getByRole("region", { name: NR_FILES.MOUNT_TITLE });
    await expect(map).toContainText("/home/agent/drive");
    await expect(map).toContainText(`${NR_FILES.MOUNT_ITEM(DRIVE.name)} ${NR_FILES.MOUNT_STARTS} ${NR_FILES.MOUNT_READ_ONLY}`);
  });

  test("an Environment opens with the drive attached and starting there", async ({ page }) => {
    const sent = await stub(page, { userDrive: DRIVE });
    await toFiles(page, "environment");
    await expect(page.getByRole("checkbox", { name: DRIVE_MEMBER.NR_CHECKBOX })).toBeChecked();
    await expect(startsHere(page, DRIVE_MEMBER.NR_CHECKBOX)).toBeChecked();
    await expect.poll(() => last(sent).drive).toMatchObject({ enabled: true });
  });

  test("a drive the profile refuses is one sentence where the checkbox would be", async ({ page }) => {
    await stub(page, { userDrive: DRIVE, deniedBy: "Contractors" });
    await toFiles(page, "application");
    await expect(page.getByTestId("nr-drive-reason")).toHaveText(DRIVE_MEMBER.NR_DENIED("Contractors"));
    await expect(page.getByRole("checkbox", { name: DRIVE_MEMBER.NR_CHECKBOX })).toHaveCount(0);
  });
});
