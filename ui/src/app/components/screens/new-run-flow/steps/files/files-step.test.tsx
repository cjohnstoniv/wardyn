/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { NR_FILES } from "../../../../wardyn/copy/new-run/files";
import { DENIED } from "../../../../../lib/permissions-copy";
import { PROVIDERS } from "../../../../../lib/workspace-providers-copy";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import { filesPreflight, limitsFor, MAIN_DRIVE_FACTS, REPO_ATTACHMENT, SCENARIO_FILES_WORKSPACES } from "../../../../../lib/new-run-fixtures/scenarios/files";
import type { RepoBranches } from "../../../../../lib/api/repo-branches";
import type { MeDrive, MeDriveCatalogue } from "../../../../../lib/api/me-drives";
import type { Workspace } from "../../../../../lib/types/workspaces";
import type { RunIntent } from "../../draft/run-intent";
import { NewRunDraftProvider, useNewRunDispatch, useNewRunDraft } from "../../new-run-route";
import { FilesStep } from "./files-step";

const wfetchMock = vi.fn();
const go = vi.fn();
vi.mock("../../../../../lib/api/core", async (orig) => ({ ...(await orig<typeof import("../../../../../lib/api/core")>()), wfetch: (...a: unknown[]) => wfetchMock(...a) }));
// The shared checks hook is faked: it owns debounce and identity fencing, which are its own tests'.
const checks = vi.hoisted(() => ({ bodies: [] as (Record<string, unknown> | null)[], result: null as unknown, principal: "alice" }));
vi.mock("../../draft/use-intent-checks", () => ({
  useDraftIdentity: () => ({ principal: checks.principal, resolved: true, revision: 0, authGeneration: 0 }),
  useIntentChecks: (params: { body: Record<string, unknown> | null }) => {
    checks.bodies.push(params.body);
    return { preflightResult: params.body === null ? null : checks.result };
  },
}));

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });
const LISTED: RepoBranches = { listing: "listed", branches: ["main", "dev"], default: "main", truncated: false };
const YOUR_DRIVE: MeDrive = {
  id: "11111111-1111-4111-8111-111111111111", name: "Your drive", section: "user", source: "personal", backend: "docker_volume",
  target: "/home/agent/drive", writable: true, enforcement: "filesystem", legacy: true,
};
const TEAM_SHARE: MeDrive = {
  id: "22222222-2222-4222-8222-222222222222", name: "Data team share", section: "shared", source: "team", backend: "host_path",
  target: "/home/agent/shared/data-team-share", writable: false, enforcement: "none", legacy: false,
};
const FINANCE: MeDrive = { ...TEAM_SHARE, id: "33333333-3333-4333-8333-333333333333", name: "Finance archive", target: "/home/agent/shared/finance-archive", unavailable_reason: "not_in_pool" };
const CATALOGUE: MeDriveCatalogue = { drives: [YOUR_DRIVE, TEAM_SHARE, FINANCE], pool_checked: true };
/** No drive of the person's own: an Environment opens with nothing chosen. */
const NO_LEGACY: MeDriveCatalogue = { drives: [TEAM_SHARE, FINANCE], pool_checked: true };
// A workspace with a repository and one host folder: adding its folder clones none of its repositories.
const FOLDER_WS: Workspace = {
  ...SCENARIO_FILES_WORKSPACES.member[0], id: "ws-data", name: "data", source: "github.com/acme/data",
  sources: [{ type: "repo", source: "github.com/acme/data", ref: "main", clone_target: "/home/agent/work/data" }, { type: "local_dir", path: "/srv/datasets", writable: true }],
};
const INTERACTIVE: RunIntent = { shape: "environment", experience: "interactive" };
const BACKGROUND: RunIntent = { shape: "application", experience: "background", workload: { kind: "command", command: "make" } };

let branches: RepoBranches | Response;
let workspaceList: Workspace[];
let catalogue: MeDriveCatalogue;
let k8s: boolean;
let drivesDown = false;
beforeEach(() => {
  workspaceList = [...SCENARIO_FILES_WORKSPACES.member, FOLDER_WS];
  catalogue = CATALOGUE;
  drivesDown = false;
  k8s = false;
  checks.result = filesPreflight(MAIN_DRIVE_FACTS.filesystem);
  branches = LISTED;
  checks.bodies.length = 0;
  checks.principal = "alice";
  go.mockReset();
  wfetchMock.mockImplementation(async (path: string) => {
    if (path.includes("/branches")) return branches instanceof Response ? branches : json(branches);
    if (path.startsWith("/workspaces")) return json(workspaceList);
    if (path.startsWith("/me/drives")) return drivesDown ? json({ error: "drives unavailable" }, 503) : json(catalogue);
    if (path.startsWith("/setup/status")) return json({ runner: { driver: "", confinement_classes: [], kubernetes: k8s } });
    throw new Error(`unexpected ${path}`);
  });
});
afterEach(() => {
  cleanup();
  wfetchMock.mockReset();
});

function Seed({ intent }: { intent: RunIntent }) {
  const dispatch = useNewRunDispatch();
  React.useEffect(() => dispatch({ type: "seed", origin: { kind: "scratch" }, intent }), [dispatch, intent]);
  return null;
}
const Drawn = () => <p data-testid="intent">{JSON.stringify(useNewRunDraft().intent)}</p>;
const drawn = (): RunIntent => JSON.parse(screen.getByTestId("intent").textContent!);
// An edit to the draft from outside the step (another screen, a document), as the reducer takes it.
const external = { edit: (_apply: (i: RunIntent) => RunIntent) => {} };
function External() {
  const dispatch = useNewRunDispatch();
  React.useEffect(() => { external.edit = (apply) => dispatch({ type: "edit", apply }); }, [dispatch]);
  return null;
}

function show(intent: RunIntent = INTERACTIVE, createUserDrive?: React.ComponentProps<typeof FilesStep>["createUserDrive"]) {
  render(
    <MemoryRouter>
      <NewRunDraftProvider owner="alice">
        <Seed intent={intent} />
        <FilesStep location={{ screen: "step", step: "files" }} go={go} startFromScratch={() => {}} createUserDrive={createUserDrive} />
        <Drawn />
        <External />
      </NewRunDraftProvider>
    </MemoryRouter>,
  );
}
const size = () => screen.getByLabelText(NR_FILES.SIZE_LABEL) as HTMLInputElement;
const continueButton = () => screen.getByRole("button", { name: NR_SHARED.CONTINUE_TO(NR_SHARED.STEP_TOOLS) });
const dialog = () => screen.getByRole("dialog");
const openAdd = async () => userEvent.click(await screen.findByRole("button", { name: NR_FILES.ADD_ARIA }));
const pickType = (title: string) => userEvent.click(within(dialog()).getByRole("button", { name: new RegExp(`^${title}`) }));
const pickSource = async (name: string) => userEvent.click(await within(dialog()).findByRole("radio", { name: new RegExp(`^${name}`) }));
const dialogButton = (name: string | RegExp) => within(dialog()).getByRole("button", { name });
/** Adds one item through Type, Choose and Mount path with the defaults. */
async function add(type: string, name: string) {
  await openAdd();
  await pickType(type);
  await pickSource(name);
  await userEvent.click(dialogButton(/^Continue/));
  await userEvent.click(dialogButton(NR_FILES.ADD_CONFIRM));
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
}
const card = (name: string) => screen.getAllByTestId("nr-attach-card").find((c) => within(c).queryByText(name, { selector: "strong" }))!;

describe("main drive", () => {
  it("shows the section, a Required chip and the pool's default, with the cap and its enforcement word", async () => {
    show();
    expect(screen.getByRole("heading", { level: 2, name: NR_FILES.MAIN_TITLE })).toBeInTheDocument();
    expect(screen.getByText(NR_FILES.MAIN_BODY)).toBeInTheDocument();
    expect(screen.getByText(NR_SHARED.REQUIRED)).toBeInTheDocument();
    expect(await screen.findByText(NR_FILES.UP_TO(8))).toBeInTheDocument();
    expect(size()).toHaveValue(4);
    expect(size()).toHaveAttribute("step", "0.1");
    expect(screen.getByText(NR_FILES.ENFORCED)).toBeInTheDocument();
    expect(screen.getByText(NR_FILES.REUSE_LABEL)).toBeInTheDocument();
    expect(screen.getByLabelText(NR_FILES.REUSE_LABEL)).toBeDisabled();
    expect(screen.getAllByText(NR_SHARED.COMING_SOON).length).toBeGreaterThanOrEqual(1);
  });

  it("clamps a size over the cap to the cap on blur and names the pool", async () => {
    show();
    await screen.findByText(NR_FILES.UP_TO(8));
    await userEvent.clear(size());
    await userEvent.type(size(), "16");
    await userEvent.tab();
    expect(size()).toHaveValue(8);
    expect(screen.getByText(NR_FILES.CLAMP_POOL(8, "Shared pool"))).toBeInTheDocument();
    expect(drawn().resources).toEqual({ disk_mib: 8192 });
  });

  it("names the server or the organisation when they, not the pool, set the cap", async () => {
    checks.result = { ...filesPreflight(MAIN_DRIVE_FACTS.filesystem), runner_pools: [{ ...filesPreflight(MAIN_DRIVE_FACTS.filesystem).runner_pools![0], limits: [limitsFor("interactive", 4096, "deployment")] }] };
    show();
    await screen.findByText(NR_FILES.UP_TO(4));
    await userEvent.clear(size());
    await userEvent.type(size(), "9");
    await userEvent.tab();
    expect(screen.getByText(NR_FILES.CLAMP_SERVER(4))).toBeInTheDocument();
  });

  it("keeps a size under the cap in whole MiB and sends it as resources.disk_mib", async () => {
    show();
    await screen.findByText(NR_FILES.UP_TO(8));
    await userEvent.clear(size());
    await userEvent.type(size(), "2.5");
    await userEvent.tab();
    expect(drawn().resources).toEqual({ disk_mib: 2560 });
    expect(screen.queryByText(/^Clamped/)).toBeNull();
  });

  it("reads a cap under 1 GiB with one decimal (A8)", async () => {
    checks.result = filesPreflight(MAIN_DRIVE_FACTS.filesystem, 512);
    show();
    expect(await screen.findByText(NR_FILES.UP_TO("0.5"))).toBeInTheDocument();
    expect(size()).toHaveValue(0.5);
  });

  it("omits the cap line when nothing caps the size, and asks for a size when nothing defaults it", async () => {
    checks.result = { ...filesPreflight(MAIN_DRIVE_FACTS.filesystem), runner_pools: [{ ...filesPreflight(MAIN_DRIVE_FACTS.filesystem).runner_pools![0], limits: [limitsFor("interactive")] }] };
    show();
    await screen.findByText(NR_FILES.ENFORCED);
    expect(screen.queryByText(/^Up to/)).toBeNull();
    expect(size()).toHaveValue(null);
    await userEvent.click(screen.getByRole("button", { name: NR_SHARED.CONTINUE_TO(NR_SHARED.STEP_TOOLS) }));
    expect(size()).toHaveFocus();
    expect(go).not.toHaveBeenCalled();
    await userEvent.tab();
    // Under the field, and in the summary.
    expect((await screen.findAllByText(NR_FILES.SIZE_ERROR)).length).toBeGreaterThanOrEqual(1);
  });

  it("with no size the summary asks for one and never reads done, and the map names the drive with no number", async () => {
    catalogue = NO_LEGACY;
    checks.result = { ...filesPreflight(MAIN_DRIVE_FACTS.filesystem), runner_pools: [{ ...filesPreflight(MAIN_DRIVE_FACTS.filesystem).runner_pools![0], limits: [limitsFor("interactive")] }] };
    show();
    await screen.findByText(NR_FILES.ENFORCED);
    await userEvent.click(screen.getByRole("radio", { name: NR_FILES.NO_EXTRA }));
    const map = screen.getByRole("region", { name: NR_FILES.MOUNT_TITLE });
    expect(within(map).getByText(NR_FILES.MAIN_TITLE)).toBeInTheDocument();
    expect(within(map).queryByText(/GiB/)).toBeNull();
    await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent(NR_FILES.SIZE_ERROR), { timeout: 3000 });
    expect(screen.getByTestId("nr-files-summary")).toHaveAttribute("data-tone", "prompt");
    await userEvent.type(size(), "4");
    await waitFor(() => expect(screen.getByTestId("nr-files-summary")).toHaveAttribute("data-tone", "done"), { timeout: 3000 });
    expect(within(map).getByText(NR_FILES.MOUNT_MAIN(4))).toBeInTheDocument();
  });
});

describe("enforcement chip (W1)", () => {
  it.each([
    ["eviction", NR_FILES.ENFORCED_BY_STOP],
    ["none", NR_FILES.NOT_ENFORCED],
    ["runner", `${NR_FILES.ENFORCED} ${NR_FILES.REPORTED_BY_RUNNER}`],
  ] as const)("%s says its own words", async (word, text) => {
    checks.result = filesPreflight(MAIN_DRIVE_FACTS[word]);
    show();
    expect(await screen.findByText(text)).toBeInTheDocument();
  });

  it("an unknown word shows no chip", async () => {
    checks.result = filesPreflight(MAIN_DRIVE_FACTS.unknown);
    show();
    await screen.findByText(NR_FILES.UP_TO(8));
    expect(screen.queryByText(NR_FILES.ENFORCED)).toBeNull();
    expect(screen.queryByText(NR_FILES.NOT_ENFORCED)).toBeNull();
  });

  it("its tooltip opens on press and carries the server's sentence", async () => {
    checks.result = filesPreflight(MAIN_DRIVE_FACTS.none);
    show();
    await userEvent.click(await screen.findByRole("button", { name: NR_FILES.NOT_ENFORCED_ARIA }));
    expect(await screen.findByText(NR_FILES.NOT_ENFORCED_TIP)).toBeInTheDocument();
  });
});

describe("what happens to it (W2)", () => {
  it("an Interactive run on a pool that keeps stopped sandboxes says how long, with its maximum", async () => {
    show();
    expect(await screen.findByText(NR_FILES.LIFE_KEPT("8 hours", "24 hours"))).toBeInTheDocument();
  });

  it("deletes it when the pool keeps nothing, and for a Background run", async () => {
    checks.result = filesPreflight(MAIN_DRIVE_FACTS.eviction);
    show();
    expect(await screen.findByText(NR_FILES.LIFE_DELETED)).toBeInTheDocument();
    cleanup();
    checks.result = filesPreflight(MAIN_DRIVE_FACTS.filesystem, 8192, "background");
    show(BACKGROUND);
    expect(await screen.findByText(NR_FILES.LIFE_DELETED)).toBeInTheDocument();
  });

  it("a run with no end is kept until you end it; before the facts arrive nothing is claimed", async () => {
    show({ ...INTERACTIVE, no_end: true });
    expect(await screen.findByText(NR_FILES.LIFE_NO_END)).toBeInTheDocument();
    cleanup();
    checks.result = null;
    show();
    expect(screen.queryByText(NR_FILES.LIFE_LABEL)).toBeNull();
  });
});

describe("the list and the Add dialog", () => {
  it("starts empty with No extra files, and Add opens on Type with five types, Your computer Coming soon", async () => {
    catalogue = NO_LEGACY;
    show();
    expect(screen.getByRole("heading", { level: 2, name: NR_FILES.ATTACH_TITLE })).toBeInTheDocument();
    expect(screen.getByRole("radio", { name: NR_FILES.NO_EXTRA })).not.toBeChecked();
    await openAdd();
    expect(within(dialog()).getByRole("heading", { name: NR_FILES.DIALOG_TITLE })).toBeInTheDocument();
    for (const t of [NR_FILES.REPOSITORY_BADGE, NR_FILES.KIND_USER_DRIVE, NR_FILES.KIND_SHARED_DRIVE, NR_FILES.FOLDER_TITLE]) {
      expect(dialogButton(new RegExp(`^${t}`))).toBeEnabled();
    }
    const computer = dialogButton(new RegExp(`^${NR_FILES.COMPUTER_TITLE}`));
    expect(computer).toBeDisabled();
    expect(within(computer).getByText(NR_SHARED.COMING_SOON)).toBeInTheDocument();
    expect(within(computer).getByText(NR_FILES.COMPUTER_SOON)).toBeInTheDocument();
  });

  it("adds a repository at a picked branch; the first thing added is where it starts, its card open", async () => {
    show(BACKGROUND);
    await openAdd();
    await pickType(NR_FILES.REPOSITORY_BADGE);
    expect(within(dialog()).getByRole("heading", { name: NR_FILES.ADD_REPO_TITLE })).toBeInTheDocument();
    await pickSource("payments");
    const picker = await within(dialog()).findByRole("combobox", { name: NR_FILES.BRANCH_ARIA("payments") });
    await waitFor(() => expect(picker).toBeEnabled());
    await userEvent.selectOptions(picker, "dev");
    expect(within(dialog()).getByRole("link", { name: NR_FILES.ADD_REPOSITORY })).toHaveAttribute("href", "/workspaces");
    await userEvent.click(dialogButton(/^Continue/));
    expect(within(dialog()).getByRole("textbox", { name: NR_FILES.STEP_MOUNT })).toHaveValue("/home/agent/work/payments");
    await userEvent.click(dialogButton(NR_FILES.ADD_CONFIRM));
    expect(drawn().workspaces).toEqual([{ workspace_id: "ws-payments", repos: [{ repo: "github.com/acme/payments", ref: "dev" }] }]);
    expect(drawn().start_folder).toEqual({ kind: "attachment", attachment: "ws-payments", repo: "github.com/acme/payments" });
    const c = card("payments");
    expect(within(c).getByText(NR_FILES.REPOSITORY_BADGE)).toBeInTheDocument();
    expect(within(c).getByText(NR_FILES.STARTS_HERE)).toBeInTheDocument();
    expect(within(c).getByText("dev")).toBeInTheDocument();
    expect(within(c).getByText(NR_FILES.LIFE_LABEL)).toBeInTheDocument();
    expect(within(c).getByText(NR_FILES.REPOS_BODY)).toBeInTheDocument();
  });

  it("falls back to a typed branch when branches cannot be listed, and says a typed ref is not checked", async () => {
    branches = { listing: "unavailable", branches: [], truncated: false, reason: "forge_not_supported" };
    show();
    await openAdd();
    await pickType(NR_FILES.REPOSITORY_BADGE);
    await pickSource("payments");
    const field = await within(dialog()).findByRole("textbox", { name: NR_FILES.BRANCH_ARIA("payments") });
    expect(within(dialog()).getByText(NR_FILES.BRANCH_FALLBACK, { exact: false })).toBeInTheDocument();
    await userEvent.clear(field);
    await userEvent.type(field, "release/1.2");
    expect(within(dialog()).getByText(NR_FILES.REF_NOT_CHECKED)).toBeInTheDocument();
    await userEvent.click(dialogButton(/^Continue/));
    await userEvent.click(dialogButton(NR_FILES.ADD_CONFIRM));
    expect(drawn().workspaces?.[0].repos).toEqual([{ repo: "github.com/acme/payments", ref: "release/1.2" }]);
  });

  it("(R) offers a repository the person cannot launch disabled with the server's sentence, and one already added as added", async () => {
    const [payments, docs] = SCENARIO_FILES_WORKSPACES.member;
    workspaceList = [{ ...payments, available_to_you: false }, { ...docs, sources: [{ ...docs.sources![0], admitted: false }] }, FOLDER_WS];
    show({ ...INTERACTIVE, workspaces: [{ workspace_id: "ws-data", repos: [{ repo: "github.com/acme/data" }] }], start_folder: { kind: "attachment", attachment: "ws-data", repo: "github.com/acme/data" } });
    await openAdd();
    await pickType(NR_FILES.REPOSITORY_BADGE);
    expect(await within(dialog()).findByRole("radio", { name: /^payments/ })).toBeDisabled();
    expect(within(dialog()).getByText(DENIED.WORKSPACE_NOT_AVAILABLE)).toBeInTheDocument();
    expect(within(dialog()).getByRole("radio", { name: /^docs/ })).toBeDisabled();
    expect(within(dialog()).getByText(PROVIDERS.CARD_NOT_ADMITTED)).toBeInTheDocument();
    expect(within(dialog()).getByRole("radio", { name: /^data/ })).toBeDisabled();
    expect(within(dialog()).getByText(NR_FILES.ALREADY_ADDED)).toBeInTheDocument();
  });

  it("Edit reopens on Mount path; a path another item holds is refused, an invalid one too, and a free one is sent as the target", async () => {
    show({
      ...INTERACTIVE,
      workspaces: [{ workspace_id: "ws-payments", repos: [{ repo: "github.com/acme/payments" }] }, { workspace_id: "ws-docs", repos: [{ repo: "github.com/acme/docs" }] }],
      start_folder: { kind: "attachment", attachment: "ws-payments", repo: "github.com/acme/payments" },
    });
    await userEvent.click(await screen.findByRole("button", { name: NR_FILES.EDIT_NAMED("docs") }));
    expect(within(dialog()).getByRole("heading", { name: NR_FILES.EDIT_NAMED("docs") })).toBeInTheDocument();
    const path = within(dialog()).getByRole("textbox", { name: NR_FILES.STEP_MOUNT });
    expect(path).toHaveValue("/home/agent/work/docs");
    // Focus starts in the open step, on its first control, not on the step list.
    expect(path).toHaveFocus();
    await userEvent.clear(path);
    await userEvent.type(path, "/home/agent/work/payments/sub");
    await userEvent.click(dialogButton(NR_FILES.SAVE));
    expect(within(dialog()).getByText(NR_FILES.PATH_TAKEN("payments", "/home/agent/work/payments"))).toBeInTheDocument();
    expect(path).toHaveFocus();
    expect(path).toHaveAttribute("aria-invalid", "true");
    await userEvent.clear(path);
    await userEvent.type(path, "/work/docs");
    await userEvent.click(dialogButton(NR_FILES.SAVE));
    expect(within(dialog()).getByText(NR_FILES.PATH_INVALID)).toBeInTheDocument();
    await userEvent.clear(path);
    await userEvent.type(path, "/home/agent/work/handbook");
    await userEvent.click(dialogButton(NR_FILES.SAVE));
    expect(drawn().workspaces?.[1]).toEqual({ workspace_id: "ws-docs", repos: [{ repo: "github.com/acme/docs" }], target: "/home/agent/work/handbook" });
    expect(within(card("docs")).getByText("handbook", { exact: false })).toBeInTheDocument();
  });

  it("Remove takes a card out, and the start moves to the first item left", async () => {
    show({
      ...INTERACTIVE,
      workspaces: [{ workspace_id: "ws-payments", repos: [{ repo: "github.com/acme/payments" }] }, { workspace_id: "ws-docs", repos: [{ repo: "github.com/acme/docs" }] }],
      start_folder: { kind: "attachment", attachment: "ws-payments", repo: "github.com/acme/payments" },
    });
    await userEvent.click(await screen.findByRole("button", { name: NR_FILES.REMOVE_NAMED("payments") }));
    expect(drawn().workspaces).toEqual([{ workspace_id: "ws-docs", repos: [{ repo: "github.com/acme/docs" }] }]);
    expect(drawn().start_folder).toEqual({ kind: "attachment", attachment: "ws-docs", repo: "github.com/acme/docs" });
  });
});

describe("coverage kept from the old list", () => {
  it("a truncated branch list ends with Other…, which opens the typed field", async () => {
    branches = { ...LISTED, truncated: true };
    show(BACKGROUND);
    await openAdd();
    await pickType(NR_FILES.REPOSITORY_BADGE);
    await pickSource("payments");
    const picker = await within(dialog()).findByRole("combobox", { name: NR_FILES.BRANCH_ARIA("payments") });
    await waitFor(() => expect(within(picker).getByRole("option", { name: NR_FILES.BRANCH_OTHER })).toBeInTheDocument());
    await userEvent.selectOptions(picker, NR_FILES.BRANCH_OTHER);
    const field = await within(dialog()).findByRole("textbox", { name: NR_FILES.BRANCH_ARIA("payments") });
    expect(within(dialog()).queryByText(NR_FILES.BRANCH_FALLBACK, { exact: false })).toBeNull();
    await userEvent.clear(field);
    await userEvent.type(field, "v2");
    expect(within(dialog()).getByText(NR_FILES.REF_NOT_CHECKED)).toBeInTheDocument();
    await userEvent.click(dialogButton(/^Continue/));
    await userEvent.click(dialogButton(NR_FILES.ADD_CONFIRM));
    expect(drawn().workspaces?.[0].repos).toEqual([{ repo: "github.com/acme/payments", ref: "v2" }]);
  });

  it("an unavailable repository already in the draft still has its card and can be taken out", async () => {
    const [payments, docs] = SCENARIO_FILES_WORKSPACES.member;
    workspaceList = [{ ...payments, available_to_you: false }, docs];
    show({ ...BACKGROUND, workspaces: [{ workspace_id: "ws-payments", repos: [{ repo: "github.com/acme/payments" }] }], start_folder: { kind: "attachment", attachment: "ws-payments", repo: "github.com/acme/payments" } });
    const remove = await screen.findByRole("button", { name: NR_FILES.REMOVE_NAMED("payments") });
    expect(remove).toBeEnabled();
    await userEvent.click(remove);
    expect(drawn().workspaces ?? []).toEqual([]);
    expect(screen.queryAllByTestId("nr-attach-card")).toHaveLength(0);
  });
});

describe("where the server starts a workspace (first target: folders before repositories)", () => {
  const TWO_FOLDERS: Workspace = { ...FOLDER_WS, sources: [...FOLDER_WS.sources!, { type: "local_dir", path: "/srv/models", target: "/home/agent/models", writable: true }] };
  const starting = (name: string) => within(card(name)).queryByText(NR_FILES.STARTS_HERE);
  const folderSelect = () => screen.findByLabelText(NR_FILES.START_FOLDER_LABEL);

  it("a start on a workspace with no repository named is its first folder, not its repository", async () => {
    show({ ...BACKGROUND, workspaces: [{ workspace_id: "ws-data", repos: [{ repo: "github.com/acme/data" }] }], start_folder: { kind: "attachment", attachment: "ws-data" } });
    await screen.findByRole("button", { name: NR_FILES.EDIT_NAMED("data") });
    expect(starting("datasets")).toBeInTheDocument();
    expect(starting("data")).toBeNull();
    // Choosing the repository names it, and the start moves; choosing the folder again sends the workspace alone.
    await userEvent.selectOptions(await folderSelect(), "ws-data/github.com/acme/data");
    expect(drawn().start_folder).toEqual({ kind: "attachment", attachment: "ws-data", repo: "github.com/acme/data" });
    expect(starting("data")).toBeInTheDocument();
    expect(starting("datasets")).toBeNull();
    await userEvent.selectOptions(await folderSelect(), "folder:ws-data:0");
    expect(drawn().start_folder).toEqual({ kind: "attachment", attachment: "ws-data" });
    expect(starting("datasets")).toBeInTheDocument();
    expect(starting("data")).toBeNull();
  });

  it("offers only a workspace's first folder as a start", async () => {
    workspaceList = [...SCENARIO_FILES_WORKSPACES.member, TWO_FOLDERS];
    show({ ...BACKGROUND, workspaces: [{ workspace_id: "ws-data", repos: [] }], start_folder: { kind: "attachment", attachment: "ws-data" } });
    await screen.findByRole("button", { name: NR_FILES.EDIT_NAMED("models") });
    const options = within(await folderSelect()).getAllByRole("option").map((o) => (o as HTMLOptionElement).value);
    expect(options).toEqual(["folder:ws-data:0"]);
    expect(starting("datasets")).toBeInTheDocument();
    expect(starting("models")).toBeNull();
  });

  it("a repository with no folder beside it is the start whether or not it is named", async () => {
    show({ ...BACKGROUND, workspaces: [{ workspace_id: "ws-payments", repos: [{ repo: "github.com/acme/payments" }] }], start_folder: { kind: "attachment", attachment: "ws-payments" } });
    await screen.findByRole("button", { name: NR_FILES.EDIT_NAMED("payments") });
    expect(starting("payments")).toBeInTheDocument();
  });
});

describe("drives (GET /me/drives)", () => {
  it("adds the person's drive by its catalogue id, at the path Wardyn assigns, narrowed to read-only when asked", async () => {
    show(BACKGROUND);
    await openAdd();
    await pickType(NR_FILES.KIND_USER_DRIVE);
    await pickSource("Your drive");
    await userEvent.click(dialogButton(/^Continue/));
    expect(within(dialog()).queryByRole("textbox")).toBeNull();
    expect(within(dialog()).getByTitle("/home/agent/drive")).toBeInTheDocument();
    await userEvent.selectOptions(within(dialog()).getByLabelText(NR_FILES.ACCESS_LABEL), NR_FILES.ACCESS_READ);
    await userEvent.click(dialogButton(NR_FILES.ADD_CONFIRM));
    expect(drawn().drives).toEqual([{ drive_id: YOUR_DRIVE.id, read_only: true }]);
    expect(drawn().drive).toBeUndefined();
    expect(drawn().start_folder).toEqual({ kind: "attachment", drive_id: YOUR_DRIVE.id });
    const c = card("Your drive");
    expect(within(c).getByText(NR_FILES.ACCESS_READ)).toBeInTheDocument();
    expect(within(c).getByText(NR_FILES.STORED_ON)).toBeInTheDocument();
    expect(within(c).getByText(NR_FILES.SOURCE_PERSONAL)).toBeInTheDocument();
    expect(within(c).getByText(NR_FILES.FAMILY_DOCKER_VOLUME)).toBeInTheDocument();
  });

  it("offers shared drives with their source and storage, a locked one as read-only by the admin, and one off this pool disabled", async () => {
    show(BACKGROUND);
    await openAdd();
    await pickType(NR_FILES.KIND_SHARED_DRIVE);
    expect(await within(dialog()).findByRole("radio", { name: /^Finance archive/ })).toBeDisabled();
    expect(within(dialog()).getByText(NR_FILES.NOT_IN_POOL)).toBeInTheDocument();
    expect(within(dialog()).getByRole("button", { name: NR_FILES.CREATE_SHARED_DRIVE })).toBeDisabled();
    await pickSource("Data team share");
    await userEvent.click(dialogButton(/^Continue/));
    expect(within(dialog()).queryByLabelText(NR_FILES.ACCESS_LABEL, { selector: "select" })).toBeNull();
    expect(within(dialog()).getByRole("button", { name: NR_FILES.LOCKED_ARIA })).toBeInTheDocument();
    await userEvent.click(dialogButton(NR_FILES.ADD_CONFIRM));
    expect(drawn().drives).toEqual([{ drive_id: TEAM_SHARE.id }]);
    expect(within(card("Data team share")).getByText(NR_FILES.KIND_SHARED_DRIVE)).toBeInTheDocument();
  });

  it("a draft carrying the older drive flag is moved to drives[] on the next drive edit, never sent beside it", async () => {
    show({ ...BACKGROUND, drive: { enabled: true }, start_folder: { kind: "attachment", attachment: "drive" } });
    expect(await screen.findByText("Your drive", { selector: "strong" })).toBeInTheDocument();
    await add(NR_FILES.KIND_SHARED_DRIVE, "Data team share");
    const after = drawn();
    expect(after.drive).toBeUndefined();
    expect(after.drives).toEqual([{ drive_id: YOUR_DRIVE.id }, { drive_id: TEAM_SHARE.id }]);
    expect(after.start_folder).toEqual({ kind: "attachment", drive_id: YOUR_DRIVE.id });
  });

  it("renders the person-created drive seam under the user drives, and only there", async () => {
    show(BACKGROUND, () => <p data-testid="create-seam">seam</p>);
    await openAdd();
    await pickType(NR_FILES.KIND_USER_DRIVE);
    expect(await within(dialog()).findByTestId("create-seam")).toBeInTheDocument();
    await userEvent.click(dialogButton(NR_SHARED.BACK));
    await pickType(NR_FILES.KIND_SHARED_DRIVE);
    expect(within(dialog()).queryByTestId("create-seam")).toBeNull();
  });

  it("says why no drive can be attached when the catalogue refuses", async () => {
    catalogue = { drives: [], denied_by_profile: "Contractors", pool_checked: false };
    show(BACKGROUND);
    await openAdd();
    await pickType(NR_FILES.KIND_USER_DRIVE);
    expect(await within(dialog()).findByTestId("nr-drive-reason")).toHaveTextContent("Contractors");
  });

  it("an Environment opens with the person's own drive attached and starting there, once (A18)", async () => {
    show({ shape: "environment", experience: "interactive" });
    await waitFor(() => expect(drawn().drives).toEqual([{ drive_id: YOUR_DRIVE.id }]));
    expect(drawn().start_folder).toEqual({ kind: "attachment", drive_id: YOUR_DRIVE.id });
    await userEvent.click(await screen.findByRole("button", { name: NR_FILES.REMOVE_NAMED("Your drive") }));
    expect(drawn().drives).toBeUndefined();
  });
});

describe("folders", () => {
  it("adds a workspace's folder with none of its repositories, at /home/agent/folders/<name>", async () => {
    show(BACKGROUND);
    await add(NR_FILES.FOLDER_TITLE, "datasets");
    expect(drawn().workspaces).toEqual([{ workspace_id: "ws-data", repos: [], target: "/home/agent/folders/datasets" }]);
    expect(drawn().start_folder).toEqual({ kind: "attachment", attachment: "ws-data" });
    const c = card("datasets");
    expect(within(c).getByText(NR_FILES.KIND_FOLDER)).toBeInTheDocument();
    expect(within(c).getByText(NR_FILES.FOLDER_BODY)).toBeInTheDocument();
    expect(within(c).getByTitle("/home/agent/folders/datasets")).toBeInTheDocument();
    await userEvent.click(within(c).getByRole("button", { expanded: true }));
    await userEvent.click(within(c).getByRole("button", { expanded: false }));
    expect(within(c).getByTitle("/srv/datasets")).toBeInTheDocument();
  });

  it("(a) shows every folder a repository's workspace brings, one card each", async () => {
    workspaceList = [...SCENARIO_FILES_WORKSPACES.member, { ...FOLDER_WS, sources: [...FOLDER_WS.sources!, { type: "local_dir", path: "/srv/models", target: "/home/agent/models", writable: true }] }];
    show(BACKGROUND);
    await add(NR_FILES.REPOSITORY_BADGE, "data");
    expect(drawn().workspaces).toEqual([{ workspace_id: "ws-data", repos: [{ repo: "github.com/acme/data" }] }]);
    expect(card("datasets")).toBeDefined();
    expect(card("models")).toBeDefined();
    expect(screen.getAllByTestId("nr-attach-card")).toHaveLength(3);
  });

  it("(b) a folder that comes with attached repositories cannot be removed, and says why on a tap", async () => {
    show({ ...BACKGROUND, workspaces: [{ workspace_id: "ws-data", repos: [{ repo: "github.com/acme/data" }] }], start_folder: { kind: "attachment", attachment: "ws-data", repo: "github.com/acme/data" } });
    const remove = await screen.findByRole("button", { name: NR_FILES.REMOVE_NAMED("datasets") });
    expect(remove).toBeDisabled();
    await userEvent.click(within(card("datasets")).getByRole("button", { name: NR_FILES.FOLDER_WITH_REPOS("data") }));
    expect(await screen.findByText(NR_FILES.FOLDER_WITH_REPOS("data"), { selector: ":not(button)" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: NR_FILES.REMOVE_NAMED("data") })).toBeEnabled();
  });

  it("(c) removing a workspace's last repository detaches it, unless the person added one of its folders", async () => {
    show(BACKGROUND);
    await add(NR_FILES.REPOSITORY_BADGE, "data");
    await userEvent.click(screen.getByRole("button", { name: NR_FILES.REMOVE_NAMED("data") }));
    expect(drawn().workspaces).toBeUndefined();
    expect(screen.queryAllByTestId("nr-attach-card")).toHaveLength(0);
    await add(NR_FILES.FOLDER_TITLE, "datasets");
    await add(NR_FILES.REPOSITORY_BADGE, "data");
    expect(screen.getByRole("button", { name: NR_FILES.REMOVE_NAMED("datasets") })).toBeDisabled();
    await userEvent.click(screen.getByRole("button", { name: NR_FILES.REMOVE_NAMED("data") }));
    expect(drawn().workspaces).toEqual([{ workspace_id: "ws-data", repos: [], target: "/home/agent/folders/datasets" }]);
    expect(card("datasets")).toBeDefined();
  });

  it("a folder set Read-only stays read-only when the last repository of its workspace is removed", async () => {
    show(BACKGROUND);
    await openAdd();
    await pickType(NR_FILES.FOLDER_TITLE);
    await pickSource("datasets");
    await userEvent.click(dialogButton(/^Continue/));
    await userEvent.selectOptions(within(dialog()).getByLabelText(NR_FILES.ACCESS_LABEL), "read");
    await userEvent.click(dialogButton(NR_FILES.ADD_CONFIRM));
    await add(NR_FILES.REPOSITORY_BADGE, "data");
    await userEvent.click(screen.getByRole("button", { name: NR_FILES.REMOVE_NAMED("data") }));
    expect(drawn().workspaces).toEqual([{ workspace_id: "ws-data", repos: [], read_only: true, target: "/home/agent/folders/datasets" }]);
    expect(within(card("datasets")).getByText(NR_FILES.ACCESS_READ)).toBeInTheDocument();
  });

  it("a folder that shares its workspace shows where the server mounts it: its source's target, else /home/agent/work", async () => {
    show(BACKGROUND);
    await add(NR_FILES.REPOSITORY_BADGE, "data");
    // No target is sent for it (the request can place only a workspace's one thing), so the server's default is its path.
    expect(drawn().workspaces).toEqual([{ workspace_id: "ws-data", repos: [{ repo: "github.com/acme/data" }] }]);
    expect(within(card("datasets")).getByTitle("/home/agent/work")).toBeInTheDocument();
    expect(within(card("datasets")).queryByTitle("/home/agent/folders/datasets")).toBeNull();
    cleanup();
    workspaceList = [...SCENARIO_FILES_WORKSPACES.member, { ...FOLDER_WS, sources: [FOLDER_WS.sources![0], { type: "local_dir", path: "/srv/datasets", target: "/home/agent/sets", writable: true }] }];
    show(BACKGROUND);
    await add(NR_FILES.REPOSITORY_BADGE, "data");
    expect(within(card("datasets")).getByTitle("/home/agent/sets")).toBeInTheDocument();
  });

  it("a folder is read-only unless its source says writable, with no choice to widen it", async () => {
    workspaceList = [...SCENARIO_FILES_WORKSPACES.member, { ...FOLDER_WS, sources: [{ type: "local_dir", path: "/srv/datasets" }] }];
    show(BACKGROUND);
    await openAdd();
    await pickType(NR_FILES.FOLDER_TITLE);
    await pickSource("datasets");
    await userEvent.click(dialogButton(/^Continue/));
    expect(within(dialog()).queryByRole("combobox", { name: NR_FILES.ACCESS_LABEL })).toBeNull();
    expect(within(dialog()).queryByRole("button", { name: NR_FILES.LOCKED_ARIA })).toBeNull();
    await userEvent.click(dialogButton(NR_FILES.ADD_CONFIRM));
    expect(drawn().workspaces).toEqual([{ workspace_id: "ws-data", repos: [], target: "/home/agent/folders/datasets" }]);
    expect(within(card("datasets")).getByText(NR_FILES.ACCESS_READ)).toBeInTheDocument();
    expect(within(card("datasets")).queryByText(NR_FILES.ACCESS_EDIT)).toBeNull();
  });

  it("says none are onboarded when no workspace has a folder (W-48)", async () => {
    workspaceList = [...SCENARIO_FILES_WORKSPACES.member];
    show(BACKGROUND);
    await openAdd();
    await pickType(NR_FILES.FOLDER_TITLE);
    expect(within(dialog()).getByText(NR_FILES.FOLDERS_NONE)).toBeInTheDocument();
    expect(within(dialog()).queryByRole("radio")).toBeNull();
  });

  it("is disabled with its reason when the server runs its sandboxes on Kubernetes", async () => {
    k8s = true;
    show(BACKGROUND);
    await openAdd();
    await waitFor(() => expect(dialogButton(new RegExp(`^${NR_FILES.FOLDER_TITLE}`))).toBeDisabled());
    expect(within(dialog()).getByText(NR_FILES.FOLDER_REMOTE)).toBeInTheDocument();
  });
});

describe("starting folder", () => {
  const TWO: RunIntent = {
    ...INTERACTIVE,
    workspaces: [{ workspace_id: "ws-payments", repos: [{ repo: "github.com/acme/payments" }] }, { workspace_id: "ws-docs", repos: [{ repo: "github.com/acme/docs" }] }],
    start_folder: { kind: "attachment", attachment: "ws-payments", repo: "github.com/acme/payments" },
  };

  it("chooses only from what was added, with a subfolder inside it", async () => {
    show(TWO);
    const folder = await screen.findByLabelText(NR_FILES.START_FOLDER_LABEL);
    await waitFor(() => expect(within(folder).getAllByRole("option")).toHaveLength(2));
    await userEvent.selectOptions(folder, "github.com/acme/docs".length ? `ws-docs/github.com/acme/docs` : "");
    expect(drawn().start_folder).toEqual({ kind: "attachment", attachment: "ws-docs", repo: "github.com/acme/docs" });
    await userEvent.type(screen.getByLabelText(new RegExp(`^${NR_FILES.SUBFOLDER_LABEL}`)), "src");
    expect(drawn().start_folder).toEqual({ kind: "attachment", attachment: "ws-docs", repo: "github.com/acme/docs", subpath: "src" });
    expect(screen.getByText("src", { selector: "span", exact: false })).toBeInTheDocument();
    await waitFor(() => expect(screen.getByText(/starts in docs\/src/)).toBeInTheDocument(), { timeout: 3000 });
  });

  it("refuses a subfolder outside its item, and a held Continue focuses it", async () => {
    show(TWO);
    const sub = await screen.findByLabelText(new RegExp(`^${NR_FILES.SUBFOLDER_LABEL}`));
    await waitFor(() => expect(sub).toBeEnabled());
    await userEvent.type(sub, "../etc");
    expect(screen.getByText(NR_FILES.SUBFOLDER_ERROR("payments"))).toBeInTheDocument();
    expect(drawn().start_folder?.subpath).toBeUndefined();
    await userEvent.click(continueButton());
    expect(sub).toHaveFocus();
    expect(go).not.toHaveBeenCalled();
  });

  it("is the Main drive, disabled, while nothing is added", async () => {
    catalogue = NO_LEGACY;
    show();
    const folder = await screen.findByLabelText(NR_FILES.START_FOLDER_LABEL);
    expect(folder).toBeDisabled();
    expect(within(folder).getByRole("option", { name: NR_FILES.START_MAIN })).toBeInTheDocument();
  });

  it("holds Continue on the folder select when something is added and nothing starts", async () => {
    show({ ...INTERACTIVE, workspaces: [{ workspace_id: "ws-docs", repos: [{ repo: "github.com/acme/docs" }] }] });
    await screen.findByRole("button", { name: NR_FILES.EDIT_NAMED("docs") });
    await userEvent.type(size(), "2");
    await userEvent.click(continueButton());
    expect(document.getElementById("nr-starts-here")).toHaveFocus();
    expect(go).not.toHaveBeenCalled();
  });
});

describe("mount map and summary", () => {
  it("lists the main drive and the server's attachment paths, marking the start, from the run's own preflight", async () => {
    checks.result = { ...filesPreflight(MAIN_DRIVE_FACTS.filesystem), attachments: [{ ...REPO_ATTACHMENT, target: "/srv/checkout/payments", starts_here: true }] };
    show({ ...INTERACTIVE, workspaces: [{ workspace_id: "ws-payments", repos: [{ repo: "github.com/acme/payments" }] }], start_folder: { kind: "attachment", attachment: "ws-payments", repo: "github.com/acme/payments" } });
    const map = screen.getByRole("region", { name: NR_FILES.MOUNT_TITLE });
    expect(await within(map).findByText("└ /srv/checkout/payments")).toBeInTheDocument();
    expect(within(map).getByText(`${NR_FILES.MOUNT_ITEM("payments")} ${NR_FILES.MOUNT_STARTS}`)).toBeInTheDocument();
    expect(within(map).getByText(NR_FILES.MOUNT_MAIN(4))).toBeInTheDocument();
    expect(within(card("payments")).getByTitle("/srv/checkout/payments")).toBeInTheDocument();
  });

  it("lists the server's rows in the cards' order", async () => {
    checks.result = {
      ...filesPreflight(MAIN_DRIVE_FACTS.filesystem, 8192, "background"),
      attachments: [
        { kind: "drive", drive_id: YOUR_DRIVE.id, name: "Your drive", target: "/home/agent/drive", read_only: false, starts_here: false },
        { ...REPO_ATTACHMENT, starts_here: true },
      ],
    };
    show({ ...BACKGROUND, workspaces: [{ workspace_id: "ws-payments", repos: [{ repo: "github.com/acme/payments" }] }], drives: [{ drive_id: YOUR_DRIVE.id }], start_folder: { kind: "attachment", attachment: "ws-payments", repo: "github.com/acme/payments" } });
    const map = screen.getByRole("region", { name: NR_FILES.MOUNT_TITLE });
    await within(map).findByText("└ /home/agent/drive");
    await screen.findByRole("button", { name: NR_FILES.EDIT_NAMED("payments") });
    expect(within(map).getAllByRole("listitem").map((li) => li.querySelector("code")?.textContent)).toEqual(["/", "└ /home/agent/work/payments", "└ /home/agent/drive"]);
  });

  it("keeps the last server answer through an edit instead of guessing a map from the draft", async () => {
    checks.result = { ...filesPreflight(MAIN_DRIVE_FACTS.filesystem), attachments: [REPO_ATTACHMENT, { kind: "drive", drive_id: "d1", name: "Notes", target: "/home/agent/drives/notes", read_only: false, starts_here: false }] };
    show({ ...INTERACTIVE, workspaces: [{ workspace_id: "ws-payments" }, { workspace_id: "ws-docs" }], start_folder: { kind: "attachment", attachment: "ws-payments" } });
    const map = screen.getByRole("region", { name: NR_FILES.MOUNT_TITLE });
    expect(await within(map).findByText("└ /home/agent/drives/notes")).toBeInTheDocument();
    // the next answer is not in yet: the hook has nothing current for this body
    checks.result = null;
    await userEvent.click(await screen.findByRole("button", { name: NR_FILES.REMOVE_NAMED("docs") }));
    expect(within(map).getByText("└ /home/agent/drives/notes")).toBeInTheDocument();
    expect(within(map).getByText("└ /home/agent/work/payments")).toBeInTheDocument();
  });

  it("drops a kept answer when the person it was for is gone", async () => {
    checks.result = { ...filesPreflight(MAIN_DRIVE_FACTS.filesystem), attachments: [REPO_ATTACHMENT] };
    show({ ...INTERACTIVE, workspaces: [{ workspace_id: "ws-payments" }, { workspace_id: "ws-docs" }], start_folder: { kind: "attachment", attachment: "ws-payments" } });
    const map = screen.getByRole("region", { name: NR_FILES.MOUNT_TITLE });
    expect(await within(map).findByText("└ /home/agent/work/payments")).toBeInTheDocument();
    checks.principal = "bob";
    checks.result = null;
    await userEvent.click(await screen.findByRole("button", { name: NR_FILES.REMOVE_NAMED("docs") }));
    expect(within(map).queryByText("└ /home/agent/work/payments")).toBeNull();
  });

  it("the summary is a sticky status that names the start and the kept drives and folders as one list", async () => {
    show(BACKGROUND);
    expect(screen.getByTestId("nr-files-summary")).toHaveAttribute("data-tone", "prompt");
    // This pool defaults no size for a Background run, and a summary with no size only asks for one.
    await userEvent.type(await screen.findByLabelText(NR_FILES.SIZE_LABEL), "4");
    await add(NR_FILES.REPOSITORY_BADGE, "payments");
    await add(NR_FILES.KIND_USER_DRIVE, "Your drive");
    await add(NR_FILES.FOLDER_TITLE, "datasets");
    await add(NR_FILES.KIND_SHARED_DRIVE, "Data team share");
    // In the order they were added, not the draft's workspaces-then-drives order.
    expect(screen.getAllByTestId("nr-attach-card").map((c) => c.querySelector("strong")?.textContent))
      .toEqual(["payments", "Your drive", "datasets", "Data team share"]);
    await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent(
      `starts in payments · ${NR_FILES.SUMMARY_KEPT_MANY("Your drive, datasets and Data team share")}`,
    ), { timeout: 3000 });
    // The icon changes with the sentence, never ahead of it.
    expect(screen.getByTestId("nr-files-summary")).toHaveAttribute("data-tone", "done");
  });
});

describe("choices and Continue", () => {
  beforeEach(() => { catalogue = NO_LEGACY; });

  it("'No extra files' writes the choice and says what is lost; adding something takes it back", async () => {
    show(BACKGROUND);
    await userEvent.click(await screen.findByRole("radio", { name: NR_FILES.NO_EXTRA }));
    expect(drawn().no_repositories_or_drives).toBe(true);
    expect(await screen.findByText(NR_FILES.NO_EXTRA_DELETED)).toBeInTheDocument();
    await add(NR_FILES.REPOSITORY_BADGE, "payments");
    expect(drawn().no_repositories_or_drives).toBeUndefined();
  });

  it("holds Continue on Add until something is added or No extra files is chosen, with the empty error, then opens Tools and access", async () => {
    show();
    await screen.findByText(NR_FILES.UP_TO(8));
    await userEvent.click(continueButton());
    expect(screen.getByRole("button", { name: NR_FILES.ADD_ARIA })).toHaveFocus();
    expect(go).not.toHaveBeenCalled();
    expect(screen.getAllByText(NR_FILES.CHOOSE_FILES).length).toBeGreaterThanOrEqual(1);
    await waitFor(() => expect(screen.getByTestId("nr-files-summary")).toHaveAttribute("data-tone", "error"), { timeout: 3000 });
    await userEvent.click(screen.getByRole("radio", { name: NR_FILES.NO_EXTRA }));
    await userEvent.click(continueButton());
    expect(go).toHaveBeenCalledWith({ screen: "step", step: "tools" });
    expect(drawn().resources).toBeUndefined();
  });

  it("sends nothing while a step's local issues are open, and sends once they close", async () => {
    show({});
    await screen.findByRole("button", { name: NR_FILES.ADD_ARIA });
    expect(checks.bodies.every((b) => b === null)).toBe(true);
    cleanup();
    checks.bodies.length = 0;
    show(BACKGROUND);
    await add(NR_FILES.REPOSITORY_BADGE, "docs");
    await waitFor(() => expect(checks.bodies.at(-1)).toMatchObject({ mode_contract: "0.9", start_folder: { attachment: "ws-docs" } }));
  });

  it("asks for the pool's facts before anything is chosen, but never sends the prefilled size as a request", async () => {
    show();
    await screen.findByText(NR_FILES.UP_TO(8));
    expect(size()).toHaveValue(4);
    await userEvent.click(screen.getByRole("radio", { name: NR_FILES.NO_EXTRA }));
    await userEvent.click(continueButton());
    expect(go).toHaveBeenCalled();
    expect(drawn().resources).toBeUndefined();
    expect(checks.bodies.at(-1)).not.toHaveProperty("resources");
  });
});

describe("the starting folder's subfolder follows the draft's start", () => {
  const subfolder = () => screen.getByLabelText(new RegExp(`^${NR_FILES.SUBFOLDER_LABEL}`)) as HTMLInputElement;
  // The starting folder's own line, not a card's Starts here chip.
  const startsHere = () => screen.getAllByText(NR_FILES.STARTS_HERE).find((e) => e.parentElement?.tagName === "P")!.parentElement as HTMLElement;

  it("an edit that replaces the starting item leaves no subfolder behind, and the next start has none", async () => {
    show({ ...INTERACTIVE, workspaces: [{ workspace_id: "ws-payments", repos: [{ repo: "github.com/acme/payments" }] }],
      start_folder: { kind: "attachment", attachment: "ws-payments", repo: "github.com/acme/payments", subpath: "notes" } });
    await waitFor(() => expect(subfolder()).toHaveValue("notes"));
    await userEvent.click(await screen.findByRole("button", { name: NR_FILES.EDIT_NAMED("payments") }));
    await userEvent.click(dialogButton(NR_SHARED.BACK));
    await pickSource("docs");
    await userEvent.click(dialogButton(/^Continue/));
    await userEvent.click(dialogButton(NR_FILES.SAVE));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(drawn().start_folder).toBeUndefined();
    expect(subfolder()).toHaveValue("");
    // The next start is automatic: the first thing added. It starts at its own top.
    await add(NR_FILES.REPOSITORY_BADGE, "data");
    expect(drawn().start_folder).toEqual({ kind: "attachment", attachment: "ws-data", repo: "github.com/acme/data" });
    expect(subfolder()).toHaveValue("");
    expect(startsHere()).not.toHaveTextContent("notes");
  });

  it("a starting drive detached from the draft leaves no subfolder: the drive the step attaches again starts at its top", async () => {
    show({ shape: "environment", experience: "interactive", drives: [{ drive_id: YOUR_DRIVE.id }],
      start_folder: { kind: "attachment", drive_id: YOUR_DRIVE.id, subpath: "notes" } });
    await waitFor(() => expect(subfolder()).toHaveValue("notes"));
    act(() => external.edit(({ drives: _d, start_folder: _s, ...rest }) => rest));
    // Nothing chosen any more: an Environment attaches the person's own drive and starts there.
    await waitFor(() => expect(drawn().start_folder).toEqual({ kind: "attachment", drive_id: YOUR_DRIVE.id }));
    expect(subfolder()).toHaveValue("");
    expect(startsHere()).not.toHaveTextContent("notes");
  });
});

describe("the dialog's create form chooses a drive by its id", () => {
  const ARCHIVE: MeDrive = { ...YOUR_DRIVE, id: "44444444-4444-4444-8444-444444444444", name: "Archive", target: "/home/agent/drives/archive", writable: false, legacy: false };
  const SCRATCH: MeDrive = { ...YOUR_DRIVE, id: "55555555-5555-4555-8555-555555555555", name: "Scratch", target: "/home/agent/drives/scratch", legacy: false };
  const LATER: MeDrive = { ...SCRATCH, id: "66666666-6666-4666-8666-666666666666", name: "Later", target: "/home/agent/drives/later" };
  // Stands in for the create form: choose a drive by id, or read the catalogue again first.
  const seam = (onPress: (ctx: { choose: (id: string) => void; reload: () => void }) => void) =>
    (ctx: { choose: (id: string) => void; reload: () => void }) => <button type="button" onClick={() => onPress(ctx)}>Seam</button>;
  const toUserDrives = async () => {
    await openAdd();
    await pickType(NR_FILES.KIND_USER_DRIVE);
  };

  it("takes the chosen drive's own access and path, not those of the drive picked before it", async () => {
    catalogue = { drives: [ARCHIVE, SCRATCH], pool_checked: true };
    show(BACKGROUND, seam((ctx) => ctx.choose(SCRATCH.id)));
    await toUserDrives();
    await pickSource("Archive");
    await userEvent.click(dialogButton("Seam"));
    await userEvent.click(dialogButton(/^Continue/));
    expect(within(dialog()).getByLabelText(NR_FILES.ACCESS_LABEL)).toHaveValue("edit");
    expect(within(dialog()).getByTitle(SCRATCH.target)).toBeInTheDocument();
    await userEvent.click(dialogButton(NR_FILES.ADD_CONFIRM));
    expect(drawn().drives).toEqual([{ drive_id: SCRATCH.id }]);
  });

  it("waits for a new drive to be listed before it can continue, then takes it as listed", async () => {
    catalogue = { drives: [ARCHIVE], pool_checked: true };
    let release = () => {};
    show(BACKGROUND, seam((ctx) => {
      // The catalogue answers only when released, so the choice is made before the drive is listed.
      wfetchMock.mockImplementation(async (path: string) => {
        if (path.startsWith("/me/drives")) {
          await new Promise<void>((r) => { release = r; });
          return json({ drives: [ARCHIVE, LATER], pool_checked: true });
        }
        if (path.startsWith("/workspaces")) return json(workspaceList);
        if (path.startsWith("/setup/status")) return json({ runner: { driver: "", confinement_classes: [], kubernetes: false } });
        throw new Error(`unexpected ${path}`);
      });
      ctx.reload();
      ctx.choose(LATER.id);
    }));
    await toUserDrives();
    await pickSource("Archive");
    await userEvent.click(dialogButton("Seam"));
    expect(dialogButton(/^Continue/)).toBeDisabled();
    expect(within(dialog()).getByRole("button", { name: NR_FILES.STEP_MOUNT })).toBeDisabled();
    act(() => release());
    await waitFor(() => expect(dialogButton(/^Continue/)).toBeEnabled());
    expect(within(dialog()).getByRole("radio", { name: /^Later/ })).toBeChecked();
    await userEvent.click(dialogButton(/^Continue/));
    expect(within(dialog()).getByLabelText(NR_FILES.ACCESS_LABEL)).toHaveValue("edit");
    await userEvent.click(dialogButton(NR_FILES.ADD_CONFIRM));
    expect(drawn().drives).toEqual([{ drive_id: LATER.id }]);
  });

  it("choosing nothing clears the pick", async () => {
    catalogue = { drives: [ARCHIVE, SCRATCH], pool_checked: true };
    show(BACKGROUND, seam((ctx) => ctx.choose("")));
    await toUserDrives();
    await pickSource("Archive");
    await userEvent.click(dialogButton("Seam"));
    expect(within(dialog()).getByRole("radio", { name: /^Archive/ })).not.toBeChecked();
    expect(dialogButton(/^Continue/)).toBeDisabled();
  });

  it("a picked drive that leaves the list is no longer picked, and Continue and Mount path are held", async () => {
    catalogue = { drives: [ARCHIVE, SCRATCH], pool_checked: true };
    // The first read without Scratch, the next with it again.
    let reads = 0;
    show(BACKGROUND, seam((ctx) => { reads += 1; catalogue = { drives: reads === 1 ? [ARCHIVE] : [ARCHIVE, SCRATCH], pool_checked: true }; ctx.reload(); }));
    await toUserDrives();
    await pickSource("Scratch");
    await userEvent.click(dialogButton(/^Continue/));
    await userEvent.click(dialogButton(NR_SHARED.BACK));
    await userEvent.click(dialogButton("Seam"));
    await waitFor(() => expect(within(dialog()).queryByRole("radio", { name: /^Scratch/ })).toBeNull());
    expect(dialogButton(/^Continue/)).toBeDisabled();
    expect(within(dialog()).getByRole("button", { name: NR_FILES.STEP_MOUNT })).toBeDisabled();
    // Listed again later, it comes back unpicked.
    await userEvent.click(dialogButton("Seam"));
    expect(await within(dialog()).findByRole("radio", { name: /^Scratch/ })).not.toBeChecked();
  });

  it("an edit of a drive that leaves the list closes: there is nothing left to edit it against", async () => {
    catalogue = { drives: [ARCHIVE, SCRATCH], pool_checked: true };
    show({ ...BACKGROUND, drives: [{ drive_id: SCRATCH.id }] }, seam((ctx) => { catalogue = { drives: [ARCHIVE], pool_checked: true }; ctx.reload(); }));
    await userEvent.click(await screen.findByRole("button", { name: NR_FILES.EDIT_NAMED("Scratch") }));
    await userEvent.click(dialogButton(NR_SHARED.BACK));
    await userEvent.click(dialogButton("Seam"));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(drawn().drives).toEqual([{ drive_id: SCRATCH.id }]);
  });
});

describe("an edit while the drive list is read again", () => {
  const ARCHIVE: MeDrive = { ...YOUR_DRIVE, id: "44444444-4444-4444-8444-444444444444", name: "Archive", target: "/home/agent/drives/archive", legacy: false };
  const SCRATCH: MeDrive = { ...YOUR_DRIVE, id: "55555555-5555-4555-8555-555555555555", name: "Scratch", target: "/home/agent/drives/scratch", legacy: false };
  const reread = (next: () => void) => (ctx: { choose: (id: string) => void; reload: () => void }) =>
    <button type="button" onClick={() => { next(); ctx.reload(); }}>Read again</button>;

  it("a read that fails keeps the edit open with what was changed; a read that answers without the drive closes it", async () => {
    catalogue = { drives: [ARCHIVE, SCRATCH], pool_checked: true };
    let reads = 0;
    show({ ...BACKGROUND, drives: [{ drive_id: SCRATCH.id }] }, reread(() => {
      reads += 1;
      drivesDown = reads === 1;
      catalogue = { drives: [ARCHIVE], pool_checked: true };
    }));
    await userEvent.click(await screen.findByRole("button", { name: NR_FILES.EDIT_NAMED("Scratch") }));
    await userEvent.selectOptions(within(dialog()).getByLabelText(NR_FILES.ACCESS_LABEL), "read");
    await userEvent.click(dialogButton(NR_SHARED.BACK));
    await userEvent.click(dialogButton("Read again"));
    expect(await within(dialog()).findByTestId("nr-drive-reason")).toBeInTheDocument();
    expect(screen.getByRole("dialog")).toBeInTheDocument();
    await userEvent.click(within(dialog()).getByRole("button", { name: NR_FILES.STEP_MOUNT }));
    expect(within(dialog()).getByLabelText(NR_FILES.ACCESS_LABEL)).toHaveValue("read");

    await userEvent.click(dialogButton(NR_SHARED.BACK));
    await userEvent.click(dialogButton("Read again"));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(drawn().drives).toEqual([{ drive_id: SCRATCH.id }]);
  });

  it("an item whose drive no list names can still go back and forth: its own source stays a choice", async () => {
    const UNLISTED = "77777777-7777-4777-8777-777777777777";
    catalogue = { drives: [ARCHIVE], pool_checked: true };
    show({ ...BACKGROUND, drives: [{ drive_id: UNLISTED }] });
    await userEvent.click(await screen.findByRole("button", { name: NR_FILES.EDIT_NAMED(UNLISTED) }));
    await userEvent.click(dialogButton(NR_SHARED.BACK));
    expect(dialogButton(/^Continue/)).toBeEnabled();
    await userEvent.click(dialogButton(/^Continue/));
    await userEvent.click(dialogButton(NR_FILES.SAVE));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(drawn().drives).toEqual([{ drive_id: UNLISTED }]);
  });
});
