/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { NR_FILES } from "../../../../wardyn/copy/new-run/files";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import { filesPreflight, limitsFor, MAIN_DRIVE_FACTS, SCENARIO_FILES_WORKSPACES } from "../../../../../lib/new-run-fixtures/scenarios/files";
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
  target: "/home/agent/drive", writable: true, enforcement: "filesystem", legacy: true, owned: false,
};
const TEAM_SHARE: MeDrive = {
  id: "22222222-2222-4222-8222-222222222222", name: "Data team share", section: "shared", source: "team", backend: "host_path",
  target: "/home/agent/shared/data-team-share", writable: false, enforcement: "none", legacy: false, owned: false,
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

describe("main drive", () => {
  it("shows the section, a Required chip and the pool's default, with the cap and its enforcement word", async () => {
    show();
    expect(screen.getByRole("heading", { level: 2, name: NR_FILES.MAIN_TITLE })).toBeInTheDocument();
    expect(screen.getByText(NR_FILES.MAIN_BODY)).toBeInTheDocument();
    expect(screen.getByText(NR_SHARED.REQUIRED)).toBeInTheDocument();
    expect(await screen.findByText(NR_FILES.UP_TO("8 GiB"))).toBeInTheDocument();
    expect(size()).toHaveValue(4);
    expect(size()).toHaveAttribute("step", "0.1");
    expect(screen.getByText(NR_FILES.ENFORCED)).toBeInTheDocument();
    expect(screen.getByText(NR_FILES.REUSE_LABEL)).toBeInTheDocument();
    expect(screen.getByLabelText(NR_FILES.REUSE_LABEL)).toBeDisabled();
    expect(screen.getAllByText(NR_SHARED.COMING_SOON).length).toBeGreaterThanOrEqual(1);
  });

  // #2860: the reuse row was a 150px grid that stacked the checkbox, label and
  // chip on three lines and floated the tooltip away. One wrapping flex row now.
  it("keeps the reuse checkbox, label, chip and tooltip in one flex row", () => {
    show();
    const row = screen.getByLabelText(NR_FILES.REUSE_LABEL).parentElement!.parentElement!;
    expect(row.className).toContain("flex");
    expect(row.className).not.toContain("grid");
    expect(within(row).getByText(NR_FILES.REUSE_LABEL)).toBeInTheDocument();
    expect(within(row).getByText(NR_SHARED.COMING_SOON)).toBeInTheDocument();
    expect(within(row).getByRole("button", { name: NR_FILES.REUSE_ARIA })).toBeInTheDocument();
  });

  it("clamps a size over the cap to the cap on blur and names the pool", async () => {
    show();
    await screen.findByText(NR_FILES.UP_TO("8 GiB"));
    await userEvent.clear(size());
    await userEvent.type(size(), "16");
    await userEvent.tab();
    expect(size()).toHaveValue(8);
    expect(screen.getByText(NR_FILES.CLAMP_POOL("8 GiB", "Shared pool"))).toBeInTheDocument();
    expect(drawn().resources).toEqual({ disk_mib: 8192 });
  });

  it("names the server or the organisation when they, not the pool, set the cap", async () => {
    checks.result = { ...filesPreflight(MAIN_DRIVE_FACTS.filesystem), limits: limitsFor("interactive", 4096, "deployment") };
    show();
    await screen.findByText(NR_FILES.UP_TO("4 GiB"));
    await userEvent.clear(size());
    await userEvent.type(size(), "9");
    await userEvent.tab();
    expect(screen.getByText(NR_FILES.CLAMP_SERVER("4 GiB"))).toBeInTheDocument();
  });

  it("prefills a size the run's policy wrote, and clamps above it naming the policy", async () => {
    checks.result = { ...filesPreflight(MAIN_DRIVE_FACTS.filesystem), main_drive: { applied_mib: 2048, basis: "policy", soft: false } };
    show();
    await screen.findByText(NR_FILES.UP_TO("2 GiB"));
    expect(size()).toHaveValue(2);
    await userEvent.clear(size());
    await userEvent.type(size(), "9");
    await userEvent.tab();
    expect(size()).toHaveValue(2);
    expect(screen.getByText("Clamped to 2 GiB by this run's policy.")).toBeInTheDocument();
    expect(drawn().resources).toEqual({ disk_mib: 2048 });
  });

  it("reads the cap and default from the run's own limits, never from a pool choice's", async () => {
    const pre = filesPreflight(MAIN_DRIVE_FACTS.filesystem);
    // A choice that also carries the pool's catalogue limits: the run's limits above it are what applies.
    checks.result = { ...pre, runner_pools: [{ ...pre.runner_pools![0], limits: [limitsFor("interactive", 2048)] }] };
    show();
    expect(await screen.findByText(NR_FILES.UP_TO("8 GiB"))).toBeInTheDocument();
    expect(size()).toHaveValue(4);
  });

  it("names the organisation when its governance sets the cap, and Continue sends the clamped size", async () => {
    checks.result = { ...filesPreflight(MAIN_DRIVE_FACTS.filesystem), limits: limitsFor("interactive", 6144, "governance") };
    show();
    await screen.findByText(NR_FILES.UP_TO("6 GiB"));
    await userEvent.clear(size());
    await userEvent.type(size(), "9");
    await userEvent.tab();
    expect(size()).toHaveValue(6);
    expect(screen.getByText(NR_FILES.CLAMP_ORG("6 GiB"))).toBeInTheDocument();
    expect(drawn().resources).toEqual({ disk_mib: 6144 });
    // Typed over the cap again and sent straight on: Continue commits the clamped size.
    await userEvent.clear(size());
    await userEvent.type(size(), "12");
    await userEvent.click(screen.getByRole("button", { name: NR_SHARED.CONTINUE_TO(NR_SHARED.STEP_TOOLS) }));
    expect(drawn().resources).toEqual({ disk_mib: 6144 });
  });

  it("sends nothing for a default the person left untouched", async () => {
    show();
    await screen.findByText(NR_FILES.UP_TO("8 GiB"));
    expect(size()).toHaveValue(4);
    expect(drawn().resources).toBeUndefined();
  });

  it("an answer with no limits, or limits for another run type, claims no cap and no default", async () => {
    const { limits: _dropped, ...older } = filesPreflight(MAIN_DRIVE_FACTS.filesystem);
    checks.result = older;
    show();
    await screen.findByText(NR_FILES.ENFORCED);
    expect(screen.queryByText(/^Up to/)).toBeNull();
    expect(size()).toHaveValue(null);
    cleanup();
    checks.result = { ...filesPreflight(MAIN_DRIVE_FACTS.filesystem), limits: limitsFor("background", 8192) };
    show();
    await screen.findByText(NR_FILES.ENFORCED);
    expect(screen.queryByText(/^Up to/)).toBeNull();
    expect(size()).toHaveValue(null);
  });

  it("keeps a size under the cap in whole MiB and sends it as resources.disk_mib", async () => {
    show();
    await screen.findByText(NR_FILES.UP_TO("8 GiB"));
    await userEvent.clear(size());
    await userEvent.type(size(), "2.5");
    await userEvent.tab();
    expect(drawn().resources).toEqual({ disk_mib: 2560 });
    expect(screen.queryByText(/^Clamped/)).toBeNull();
  });

  it("reads a cap under 1 GiB with one decimal (A8)", async () => {
    checks.result = filesPreflight(MAIN_DRIVE_FACTS.filesystem, 512);
    show();
    expect(await screen.findByText(NR_FILES.UP_TO("0.5 GiB"))).toBeInTheDocument();
    expect(size()).toHaveValue(0.5);
  });

  it("omits the cap line when nothing caps the size, and asks for a size when nothing defaults it", async () => {
    checks.result = { ...filesPreflight(MAIN_DRIVE_FACTS.filesystem), limits: limitsFor("interactive") };
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
    checks.result = { ...filesPreflight(MAIN_DRIVE_FACTS.filesystem), limits: limitsFor("interactive") };
    show();
    await screen.findByText(NR_FILES.ENFORCED);
    await userEvent.click(screen.getByRole("checkbox", { name: NR_FILES.NO_EXTRA }));
    const map = screen.getByRole("region", { name: NR_FILES.MOUNT_TITLE });
    expect(within(map).getByText(NR_FILES.MAIN_TITLE)).toBeInTheDocument();
    expect(within(map).queryByText(/GiB/)).toBeNull();
    await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent(NR_FILES.SIZE_ERROR), { timeout: 3000 });
    expect(screen.getByTestId("nr-files-summary")).toHaveAttribute("data-tone", "prompt");
    await userEvent.type(size(), "4");
    await waitFor(() => expect(screen.getByTestId("nr-files-summary")).toHaveAttribute("data-tone", "done"), { timeout: 3000 });
    expect(within(map).getByText(NR_FILES.MOUNT_MAIN("4 GiB"))).toBeInTheDocument();
  });
});

describe("a size under 0.1 GiB (DR-0160)", () => {
  const exact = () => screen.queryByTestId("nr-main-drive-exact");

  it("shows a valid 32 MiB size in whole MiB, never as zero, and stores it", async () => {
    show();
    await screen.findByText(NR_FILES.UP_TO("8 GiB"));
    await userEvent.clear(size());
    await userEvent.type(size(), "0.03125");
    await userEvent.tab();
    expect(size()).toHaveValue(32);
    expect(size()).toHaveAttribute("step", "1");
    expect(screen.getByText(NR_SHARED.MIB("").trim())).toBeInTheDocument();
    expect(drawn().resources).toEqual({ disk_mib: 32 });
    // The field says the exact size, so nothing more needs saying.
    expect(exact()).toBeNull();
    // The map and the summary never read it as zero.
    expect(within(screen.getByRole("region", { name: NR_FILES.MOUNT_TITLE })).getByText(NR_FILES.MOUNT_MAIN("32 MiB"))).toBeInTheDocument();
  });

  // A stored size comes back when the field is emptied, so a later size replaces the text outright.
  const enter = (value: string) => {
    act(() => size().focus());
    fireEvent.change(size(), { target: { value } });
  };

  it("reads the field's number in the unit beside it", async () => {
    show();
    await screen.findByText(NR_FILES.UP_TO("8 GiB"));
    await userEvent.clear(size());
    await userEvent.type(size(), "0.03125");
    await userEvent.tab();
    enter("50");
    await userEvent.tab();
    expect(drawn().resources).toEqual({ disk_mib: 50 });
    // A size back over 0.1 GiB returns to GiB.
    enter("2048");
    await userEvent.tab();
    expect(drawn().resources).toEqual({ disk_mib: 2048 });
    expect(size()).toHaveValue(2);
    expect(size()).toHaveAttribute("step", "0.1");
  });

  it("states the exact MiB when the field's GiB text is rounded", async () => {
    show();
    await screen.findByText(NR_FILES.UP_TO("8 GiB"));
    await userEvent.clear(size());
    await userEvent.type(size(), "0.15");
    await userEvent.tab();
    expect(drawn().resources).toEqual({ disk_mib: 154 });
    expect(size()).toHaveValue(0.2);
    expect(exact()).toHaveTextContent(NR_FILES.SIZE_EXACT(154));
    // 0.1 GiB is 102.4 MiB, stored as 102: below 0.1 GiB, so shown in MiB.
    enter("0.1");
    await userEvent.tab();
    expect(drawn().resources).toEqual({ disk_mib: 102 });
    expect(size()).toHaveValue(102);
  });

  it("a cap of 1000 MiB reads 1000 MiB, and Continue keeps the clamped 1000 MiB with its note and caption", async () => {
    checks.result = filesPreflight(MAIN_DRIVE_FACTS.filesystem, 1000);
    show();
    expect(await screen.findByText(NR_FILES.UP_TO("1000 MiB"))).toBeInTheDocument();
    await userEvent.clear(size());
    await userEvent.type(size(), "2");
    await userEvent.tab();
    expect(size()).toHaveValue(0.9);
    expect(screen.getByText(NR_FILES.CLAMP_POOL("1000 MiB", "Shared pool"))).toBeInTheDocument();
    expect(exact()).toHaveTextContent(NR_FILES.SIZE_EXACT(1000));
    expect(drawn().resources).toEqual({ disk_mib: 1000 });
    // The summary and the mount map read the stored size exactly, never rounded to GiB.
    expect(await screen.findByText(NR_FILES.MOUNT_MAIN("1000 MiB"))).toBeInTheDocument();
    expect(screen.queryByText(NR_FILES.MOUNT_MAIN("1 GiB"))).toBeNull();
    expect(screen.queryByText(NR_FILES.MOUNT_MAIN("0.9 GiB"))).toBeNull();
    await waitFor(() => expect(document.body).toHaveTextContent(NR_FILES.SUMMARY_MAIN("1000 MiB")));

    // The unedited second commit: re-reading "0.9" would store 922 MiB and drop the note.
    await userEvent.click(screen.getByRole("button", { name: NR_SHARED.CONTINUE_TO(NR_SHARED.STEP_TOOLS) }));
    expect(drawn().resources).toEqual({ disk_mib: 1000 });
    expect(screen.getByText(NR_FILES.CLAMP_POOL("1000 MiB", "Shared pool"))).toBeInTheDocument();
    expect(exact()).toHaveTextContent(NR_FILES.SIZE_EXACT(1000));
    expect(go).toHaveBeenCalledTimes(1);
  });

  // R2-L1: while the size is being typed, the summary follows the text in its own unit.
  it("while a size is typed in MiB, the mount map reads that text, not its parsed GiB", async () => {
    show();
    await screen.findByText(NR_FILES.UP_TO("8 GiB"));
    await userEvent.clear(size());
    await userEvent.type(size(), "0.03125");
    await userEvent.tab();
    expect(size()).toHaveValue(32);
    // 512 MiB is half a GiB: once stored it reads "0.5 GiB", but while typed in MiB it reads as typed.
    enter("512");
    expect(await screen.findByText(NR_FILES.MOUNT_MAIN("512 MiB"))).toBeInTheDocument();
    expect(screen.queryByText(NR_FILES.MOUNT_MAIN("0.5 GiB"))).toBeNull();
    await userEvent.tab();
    expect(drawn().resources).toEqual({ disk_mib: 512 });
    expect(await screen.findByText(NR_FILES.MOUNT_MAIN("0.5 GiB"))).toBeInTheDocument();
  });

  it("emptying the field and leaving it keeps the stored size and the clamp note", async () => {
    checks.result = filesPreflight(MAIN_DRIVE_FACTS.filesystem, 1000);
    show();
    await screen.findByText(NR_FILES.UP_TO("1000 MiB"));
    await userEvent.clear(size());
    await userEvent.type(size(), "2");
    await userEvent.tab();
    expect(drawn().resources).toEqual({ disk_mib: 1000 });
    await userEvent.clear(size());
    expect(size()).toHaveValue(null);
    // The clamp note still describes the stored size while the field is empty.
    expect(screen.getByText(NR_FILES.CLAMP_POOL("1000 MiB", "Shared pool"))).toBeInTheDocument();
    await userEvent.tab();
    expect(size()).toHaveValue(0.9);
    expect(drawn().resources).toEqual({ disk_mib: 1000 });
    expect(screen.getByText(NR_FILES.CLAMP_POOL("1000 MiB", "Shared pool"))).toBeInTheDocument();
    expect(exact()).toHaveTextContent(NR_FILES.SIZE_EXACT(1000));
    await userEvent.click(screen.getByRole("button", { name: NR_SHARED.CONTINUE_TO(NR_SHARED.STEP_TOOLS) }));
    expect(drawn().resources).toEqual({ disk_mib: 1000 });
    expect(go).toHaveBeenCalledTimes(1);
  });

  it("a second blur of an unedited field keeps the stored size", async () => {
    checks.result = filesPreflight(MAIN_DRIVE_FACTS.filesystem, 1000);
    show();
    await screen.findByText(NR_FILES.UP_TO("1000 MiB"));
    await userEvent.clear(size());
    await userEvent.type(size(), "2");
    await userEvent.tab();
    await userEvent.click(size());
    await userEvent.tab();
    expect(drawn().resources).toEqual({ disk_mib: 1000 });
  });
});

describe("a filled size follows the pool", () => {
  // A pool with a 40 GiB cap and `mib` as its default.
  const withDefault = (mib: number) => ({
    ...filesPreflight(MAIN_DRIVE_FACTS.filesystem),
    limits: { ...limitsFor("interactive", 40960), disk_mib: { default: { value: mib }, cap: { value: 40960 }, cap_source: "pool" as const } },
  });

  it("another pool's default replaces a size the person never changed", async () => {
    checks.result = withDefault(20480);
    show();
    await waitFor(() => expect(size()).toHaveValue(20));
    // The run is moved to a pool whose default is 10 GiB.
    checks.result = withDefault(10240);
    act(() => external.edit((i) => ({ ...i, runner_pool_id: "pool-b" })));
    await waitFor(() => expect(size()).toHaveValue(10));
    expect(drawn().resources).toBeUndefined();
  });

  it("another pool's default with the same digits in another unit switches the unit too", async () => {
    checks.result = withDefault(32);
    show();
    await waitFor(() => expect(size()).toHaveValue(32));
    expect(screen.getByText("MiB")).toBeInTheDocument();
    checks.result = withDefault(32768);
    act(() => external.edit((i) => ({ ...i, runner_pool_id: "pool-b" })));
    await waitFor(() => expect(screen.getByText("GiB")).toBeInTheDocument());
    expect(size()).toHaveValue(32);
    expect(screen.queryByText("MiB")).toBeNull();
  });

  it("a pool with no size to offer clears a size the person never changed", async () => {
    checks.result = withDefault(20480);
    show();
    await waitFor(() => expect(size()).toHaveValue(20));
    // The run is moved to a pool with no default and no cap: an untouched run gets no size there.
    const open = withDefault(0);
    const none = { value: 0, unlimited: true };
    open.limits.disk_mib = { default: none, cap: none, cap_source: "pool" };
    checks.result = open;
    act(() => external.edit((i) => ({ ...i, runner_pool_id: "pool-b" })));
    await waitFor(() => expect(size()).toHaveValue(null));
    expect(drawn().resources).toBeUndefined();
  });

  it("keeps a size the person typed when the pool changes", async () => {
    checks.result = withDefault(20480);
    show();
    await waitFor(() => expect(size()).toHaveValue(20));
    await userEvent.clear(size());
    await userEvent.type(size(), "6");
    checks.result = withDefault(10240);
    act(() => external.edit((i) => ({ ...i, runner_pool_id: "pool-b" })));
    expect(size()).toHaveValue(6);
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
    await screen.findByText(NR_FILES.UP_TO("8 GiB"));
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
