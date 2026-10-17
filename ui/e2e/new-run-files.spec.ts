/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New Run, step 4 Files (public half): the main drive's size and the cap that
// clamps it, the Add dialog with its branch picker and text fallback, the
// starting folder, the mount map and the live summary, "No extra files", and
// the person's own drive from the /me/drives catalogue. Member create belongs
// to the private drives spec.
//
// The seeded backend runs no runner and holds no pool or repository, so the
// workspace list, the branch list and the preflight (whose answer carries the
// pool's limits and the mount paths) are stubbed. The preflight is computed
// from the body it was sent, so the mount map follows what the person chose.
import type { Page } from "@playwright/test";
import { test, expect, gotoConsole, navToRoute } from "./fixtures";
import { NR_DOCUMENT } from "../src/app/components/wardyn/copy/new-run/document";
import { NR_REVIEW } from "../src/app/components/wardyn/copy/new-run/review";
import { NR_FILES } from "../src/app/components/wardyn/copy/new-run/files";
import { NR_SHAPE } from "../src/app/components/wardyn/copy/new-run/shape";
import { NR_SHARED } from "../src/app/components/wardyn/copy/new-run/shared";
import { NR_TOOLS } from "../src/app/components/wardyn/copy/new-run/tools";
import { NR_START } from "../src/app/components/wardyn/copy/new-run/start";
import { DRIVE_MEMBER } from "../src/app/lib/user-drives-copy";

const WS_ID = "f1111111-1111-4111-8111-111111111111";
const POOL = { id: "f2222222-2222-4222-8222-222222222222", name: "General", scope: "organization" };
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
const DRIVE = {
  id: "f3333333-3333-4333-8333-333333333333", name: "Scratch", section: "user", source: "personal", backend: "docker_volume",
  target: "/home/agent/drive", writable: true, enforcement: "filesystem", legacy: true,
};
const CAP_MIB = 20480;
const DEFAULT_MIB = 10240;

type Body = { workspaces?: { workspace_id: string; repos?: { repo: string; ref?: string }[] }[]; drive?: object; drives?: { drive_id: string; read_only?: boolean }[]; start_folder?: { attachment?: string; repo?: string; drive_id?: string }; resources?: { disk_mib?: number }; no_repositories_or_drives?: boolean; experience?: string };

const unlimited = { default: { value: 0, unlimited: true }, cap: { value: 0, unlimited: true } };
const limitsFor = (run_type: string, capSource: string, capMib = CAP_MIB) => ({
  run_type,
  cpu_millis: unlimited,
  memory_mib: unlimited,
  disk_mib: { default: { value: Math.min(DEFAULT_MIB, capMib) }, cap: { value: capMib }, default_source: "pool", cap_source: capSource },
  lifetime_sec: unlimited,
  lifetime_span_sec: unlimited,
  lifetime_lease_sec: unlimited,
  no_end_allowed: true,
  concurrency: [],
});

/** What the server would answer for `body`: the pool's limits, the main drive's facts and the mounts it chose. */
function preflightFor(body: Body, capSource: string, capMib = CAP_MIB) {
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
  for (const d of body.drives ?? []) {
    attachments.push({ kind: "drive", drive_id: d.drive_id, name: DRIVE.name, target: DRIVE.target, read_only: d.read_only === true, starts_here: body.start_folder?.drive_id === d.drive_id });
  }
  return {
    setup_items: [],
    enforced_confinement_class: "CC2",
    runner_pool: { ...POOL, revision: 1, selection: "organisation_default" },
    // As the dry door draws it: the run's limits at the top level, the pool choices without limits.
    runner_pools: [{ ...POOL, availability: "available", main_drive: { enforcement: "filesystem", basis: "substrate", keep: { supported: false }, drive_mounts: true } }],
    limits: limitsFor(body.experience ?? "interactive", capSource, capMib),
    main_drive: body.resources?.disk_mib ? { requested_mib: body.resources.disk_mib, applied_mib: body.resources.disk_mib, basis: "request", soft: false } : { applied_mib: DEFAULT_MIB, basis: "default", default_source: "pool", soft: false },
    attachments,
  };
}

interface Opts {
  capSource?: string;
  /** The pool's disk cap in MiB; 20 GiB unless given. */
  capMib?: number;
  /** A branch listing; absent makes the list unavailable, so the text field shows. */
  branches?: string[];
  /** The person's drive catalogue; none unless given. */
  drives?: object[];
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
    return route.fulfill({ json: preflightFor(body, opts.capSource ?? "pool", opts.capMib) });
  });
  await page.route("**/api/v1/me/drives*", (route) =>
    route.fulfill({ json: { drives: opts.drives ?? [], pool_checked: true, ...(opts.deniedBy ? { denied_by_profile: opts.deniedBy } : {}) } }),
  );
  return sent;
}

const press = (page: Page, name: string) => page.getByRole("button", { name, exact: true }).click();
const next = (page: Page, step: string) => press(page, NR_SHARED.CONTINUE_TO(step));
const sizeInput = (page: Page) => page.locator("#nr-main-drive-size");
const dialog = (page: Page) => page.getByRole("dialog");
const noExtra = (page: Page) => page.getByRole("radio", { name: NR_FILES.NO_EXTRA });
const startFolder = (page: Page) => page.locator("#nr-starts-here");
const cards = (page: Page) => page.getByTestId("nr-attach-card");

/** Opens Add, picks the type and one of its sources: the dialog is left on Choose. */
async function pick(page: Page, type: string, name: string) {
  await press(page, NR_FILES.ADD_ARIA);
  await dialog(page).getByRole("button", { name: new RegExp(`^${type}`) }).click();
  await dialog(page).getByRole("radio", { name: new RegExp(`^${name}`) }).check();
}

/** Choose's Continue, then Add on Mount path. */
async function confirm(page: Page) {
  await dialog(page).getByRole("button", { name: NR_SHARED.CONTINUE, exact: true }).click();
  await dialog(page).getByRole("button", { name: NR_FILES.ADD_CONFIRM, exact: true }).click();
  await expect(dialog(page)).toBeHidden();
}

async function addRepo(page: Page, name: string) {
  await pick(page, NR_FILES.REPOSITORY_BADGE, name);
  await confirm(page);
}
// Empty until the first check is sent: a poll then reads undefined fields and tries again.
const last = (sent: Body[]): Body => sent.at(-1) ?? {};

/** Guided to the Files step for a workload. An Application needs its command on the Run step. */
async function toFiles(page: Page, workload: "environment" | "application" = "environment") {
  await gotoConsole(page);
  await navToRoute(page, "/runs/new");
  await page.getByRole("button", { name: NR_START.GUIDED_FOOT }).click();
  // A workload button writes the shape and opens Run.
  await press(page, workload === "environment" ? NR_SHAPE.ENV_TITLE : NR_SHAPE.APP_TITLE);
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
      await expect(page.getByText(NR_FILES.UP_TO("20 GiB"))).toBeVisible();
      await expect(page.getByText(NR_FILES.ENFORCED, { exact: true })).toBeVisible();
      await expect.poll(() => sent.length).toBeGreaterThan(0);
      expect(last(sent)).not.toHaveProperty("resources");

      await sizeInput(page).fill("50");
      await sizeInput(page).blur();
      await expect(sizeInput(page)).toHaveValue("20");
      await expect(page.getByText(sentence("20 GiB"))).toBeVisible();
      await expect.poll(() => last(sent).resources?.disk_mib).toBe(CAP_MIB);
    });
  }

  test("a size under 0.1 GiB is shown in whole MiB and sent as typed (DR-0160)", async ({ page }) => {
    const sent = await stub(page);
    await toFiles(page);
    await sizeInput(page).fill("0.03125");
    await sizeInput(page).blur();
    await expect(sizeInput(page)).toHaveValue("32");
    await expect(page.getByText("MiB", { exact: true })).toBeVisible();
    await expect.poll(() => last(sent).resources?.disk_mib).toBe(32);
  });

  test("a cap that is not a whole tenth of a GiB reads in MiB, and Continue keeps the clamped size (DR-0160)", async ({ page }) => {
    const sent = await stub(page, { capMib: 1000 });
    await toFiles(page);
    await expect(page.getByText(NR_FILES.UP_TO("1000 MiB"))).toBeVisible();
    await sizeInput(page).fill("2");
    await sizeInput(page).blur();
    await expect(sizeInput(page)).toHaveValue("0.9");
    await expect(page.getByText(NR_FILES.CLAMP_POOL("1000 MiB", POOL.name))).toBeVisible();
    await expect(page.getByText(NR_FILES.SIZE_EXACT(1000))).toBeVisible();

    // Continue does not read the rounded 0.9 back as 922 MiB.
    await noExtra(page).check();
    await next(page, NR_SHARED.STEP_TOOLS);
    await expect(page.getByRole("heading", { name: NR_TOOLS.TITLE })).toBeVisible();
    // Later checks may carry no size; of those that do, the last is the clamped 1000 and none is the rounded 922.
    const sizes = () => sent.map((b) => b.resources?.disk_mib).filter((v) => v !== undefined);
    await expect.poll(() => sizes().at(-1)).toBe(1000);
    expect(sizes()).not.toContain(922);
  });

  test("an emptied size holds Continue and asks for one", async ({ page }) => {
    await stub(page);
    await toFiles(page);
    await sizeInput(page).fill("");
    await sizeInput(page).blur();
    // Under the field, and in the summary, which asks for the size too.
    await expect(page.getByText(NR_FILES.SIZE_ERROR).first()).toBeVisible();
    await expect(page.getByTestId("nr-files-summary")).toHaveText(NR_FILES.SIZE_ERROR);
    await next(page, NR_SHARED.STEP_TOOLS);
    await expect(sizeInput(page)).toBeFocused();
    await expect(page).toHaveURL(/step=files/);
  });
});

test.describe("New Run, Files: repositories", () => {
  test("the branch picker lists the repository's branches and sends a choice", async ({ page }) => {
    const sent = await stub(page, { branches: ["main", "release", "dev"] });
    await toFiles(page);
    await pick(page, NR_FILES.REPOSITORY_BADGE, "web");
    const branch = dialog(page).getByRole("combobox", { name: NR_FILES.BRANCH_ARIA("web") });
    await expect(branch.locator("option")).toHaveText(["main", "release", "dev"]);
    await branch.selectOption("release");
    await confirm(page);
    await expect.poll(() => last(sent).workspaces?.[0].repos).toEqual([{ repo: REPOS[0].repo, ref: "release" }]);
  });

  test("a branch list that cannot be read falls back to a text field that says it is not checked", async ({ page }) => {
    const sent = await stub(page);
    await toFiles(page);
    await pick(page, NR_FILES.REPOSITORY_BADGE, "web");
    const field = dialog(page).getByRole("textbox", { name: NR_FILES.BRANCH_ARIA("web") });
    await expect(field).toHaveAttribute("placeholder", NR_FILES.BRANCH_PLACEHOLDER);
    await expect(dialog(page).getByText(NR_FILES.BRANCH_FALLBACK)).toBeVisible();
    // The field opens on the workspace's own ref, which is as unchecked as a typed one; emptied, nothing is claimed.
    await expect(field).toHaveValue("main");
    await field.fill("");
    await expect(dialog(page).getByText(NR_FILES.REF_NOT_CHECKED)).toHaveCount(0);

    await field.fill("feature/x");
    await expect(dialog(page).getByText(NR_FILES.REF_NOT_CHECKED)).toBeVisible();
    await confirm(page);
    await expect.poll(() => last(sent).workspaces?.[0].repos).toEqual([{ repo: REPOS[0].repo, ref: "feature/x" }]);
  });

  test("the run starts in the first thing added, the starting folder moves it, and a held Continue points at Add", async ({ page }) => {
    const sent = await stub(page, { branches: ["main"] });
    await toFiles(page);
    // Nothing is added: there is nowhere to start but the main drive, and Continue stays on Add.
    await expect(sizeInput(page)).toHaveValue("10");
    await expect(startFolder(page)).toBeDisabled();
    await next(page, NR_SHARED.STEP_TOOLS);
    await expect(page.getByRole("button", { name: NR_FILES.ADD_ARIA })).toBeFocused();
    await expect(page.getByText(NR_FILES.CHOOSE_FILES).first()).toBeVisible();
    await expect(page).toHaveURL(/step=files/);
    await expect.poll(() => sent.length).toBeGreaterThan(0);
    expect(last(sent)).not.toHaveProperty("start_folder");

    await addRepo(page, "web");
    await expect(cards(page).filter({ hasText: "web" })).toContainText(NR_FILES.STARTS_HERE);
    await expect.poll(() => last(sent).start_folder).toEqual({ kind: "attachment", attachment: WS_ID, repo: REPOS[0].repo });
    expect(last(sent).workspaces?.[0].repos).toEqual([{ repo: REPOS[0].repo }]);

    // A second one is attached and nothing more: the start stays until the person moves it.
    await addRepo(page, "docs");
    await expect.poll(() => last(sent).workspaces?.[0].repos).toHaveLength(2);
    expect(last(sent).start_folder).toEqual({ kind: "attachment", attachment: WS_ID, repo: REPOS[0].repo });
    await startFolder(page).selectOption(`${WS_ID}/${REPOS[1].repo}`);
    await expect.poll(() => last(sent).start_folder).toEqual({ kind: "attachment", attachment: WS_ID, repo: REPOS[1].repo });
    expect(last(sent).workspaces).toHaveLength(1);
  });

  test("the mount map and the summary follow what is added and where it starts", async ({ page }) => {
    await stub(page, { branches: ["main"] });
    await toFiles(page);
    await expect(page.getByRole("status").filter({ hasText: NR_FILES.CHOOSE_FILES })).toBeVisible();

    await addRepo(page, "web");
    const map = page.getByRole("region", { name: NR_FILES.MOUNT_TITLE });
    await expect(map).toContainText(NR_FILES.MOUNT_MAIN("10 GiB"));
    await expect(map).toContainText(`└ ${REPOS[0].target}`);
    await expect(map).toContainText(`${NR_FILES.MOUNT_ITEM("web")} ${NR_FILES.MOUNT_STARTS}`);

    const summary = [NR_FILES.SUMMARY_MAIN("10 GiB"), NR_FILES.SUMMARY_STARTS("web"), NR_FILES.SUMMARY_DELETED].reduce((all, p) => NR_SHARED.JOIN(all, p));
    await expect(page.getByRole("status").filter({ hasText: summary })).toBeVisible();
  });

  test("No extra files is a choice: only the main drive, and a repository added later replaces it", async ({ page }) => {
    const sent = await stub(page, { branches: ["main"] });
    await toFiles(page);
    await noExtra(page).check();
    await expect.poll(() => last(sent).no_repositories_or_drives).toBe(true);
    const nothing = NR_FILES.SUMMARY_NO_EXTRA("10 GiB", NR_FILES.SUMMARY_DELETED);
    await expect(page.getByRole("status").filter({ hasText: nothing })).toBeVisible();
    await expect(page.getByRole("region", { name: NR_FILES.MOUNT_TITLE }).getByText(/←/)).toHaveCount(0);

    await addRepo(page, "docs");
    // The choice gives way to the list.
    await expect(noExtra(page)).toHaveCount(0);
    await expect.poll(() => last(sent).no_repositories_or_drives).toBeUndefined();
    await expect.poll(() => last(sent).workspaces?.[0].repos).toEqual([{ repo: REPOS[1].repo }]);
  });
});

test.describe("New Run, Files: an unnamed workspace start (DR-0253)", () => {
  test("with one of two repositories cloned and no repository named, the run starts in that one", async ({ page }) => {
    await stub(page, { branches: ["main"] });
    const intent = {
      shape: "environment", experience: "interactive",
      workspaces: [{ workspace_id: WS_ID, repos: [{ repo: REPOS[1].repo }] }],
      start_folder: { kind: "attachment", attachment: WS_ID },
    };
    await page.route("**/api/v1/templates/import", (route) =>
      route.fulfill({ json: { document: { api_version: "wardyn/v1", kind: "Run", intent }, diagnostics: [] } }),
    );
    await gotoConsole(page);
    await navToRoute(page, "/runs/new");
    await press(page, NR_START.DOC_YAML);
    await page.getByRole("textbox", { name: NR_DOCUMENT.EDITOR_ARIA("YAML") }).fill("api_version: wardyn/v1\nkind: Run\nintent:\n  shape: environment\n");
    await press(page, NR_DOCUMENT.CONTINUE_REVIEW);
    await expect(page.getByRole("heading", { name: NR_REVIEW.TITLE })).toBeVisible();
    await press(page, `${NR_REVIEW.CHANGE} ${NR_REVIEW.GROUP_WORK}`);
    await press(page, NR_REVIEW.EDIT_FILES);
    await expect(page.getByRole("heading", { name: NR_FILES.TITLE })).toBeVisible();
    await expect(cards(page).filter({ hasText: "docs" })).toContainText(NR_FILES.STARTS_HERE);
  });
});

test.describe("New Run, Files: the person's drive (public)", () => {
  test("a drive is attached by its catalogue id; read-only and starting there reach the wire and the map", async ({ page }) => {
    const sent = await stub(page, { drives: [DRIVE] });
    await toFiles(page, "application");
    await expect(noExtra(page)).not.toBeChecked();
    await expect(cards(page)).toHaveCount(0);

    await pick(page, NR_FILES.KIND_USER_DRIVE, DRIVE.name);
    await dialog(page).getByRole("button", { name: NR_SHARED.CONTINUE, exact: true }).click();
    // Wardyn assigns a drive's path: it is shown, not typed.
    await expect(dialog(page).getByRole("textbox")).toHaveCount(0);
    await dialog(page).getByLabel(NR_FILES.ACCESS_LABEL).selectOption({ label: NR_FILES.ACCESS_READ });
    await dialog(page).getByRole("button", { name: NR_FILES.ADD_CONFIRM, exact: true }).click();
    await expect.poll(() => last(sent).drives).toEqual([{ drive_id: DRIVE.id, read_only: true }]);
    expect(last(sent)).not.toHaveProperty("drive");
    expect(last(sent).start_folder).toEqual({ kind: "attachment", drive_id: DRIVE.id });

    const map = page.getByRole("region", { name: NR_FILES.MOUNT_TITLE });
    await expect(map).toContainText(DRIVE.target);
    await expect(map).toContainText(`${NR_FILES.MOUNT_ITEM(DRIVE.name)} ${NR_FILES.MOUNT_STARTS} ${NR_FILES.MOUNT_READ_ONLY}`);
  });

  test("an Environment opens with the drive attached and starting there", async ({ page }) => {
    const sent = await stub(page, { drives: [DRIVE] });
    await toFiles(page, "environment");
    await expect(cards(page).filter({ hasText: DRIVE.name })).toContainText(NR_FILES.STARTS_HERE);
    await expect.poll(() => last(sent).drives).toEqual([{ drive_id: DRIVE.id }]);
    expect(last(sent).start_folder).toEqual({ kind: "attachment", drive_id: DRIVE.id });
  });

  test("a drive the profile refuses is one sentence where the drives would be", async ({ page }) => {
    await stub(page, { deniedBy: "Contractors" });
    await toFiles(page, "application");
    await press(page, NR_FILES.ADD_ARIA);
    await dialog(page).getByRole("button", { name: new RegExp(`^${NR_FILES.KIND_USER_DRIVE}`) }).click();
    await expect(dialog(page).getByTestId("nr-drive-reason")).toHaveText(DRIVE_MEMBER.NR_DENIED("Contractors"));
    await expect(dialog(page).getByRole("radio")).toHaveCount(0);
  });
});
