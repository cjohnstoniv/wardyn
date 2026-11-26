/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New Run, Files: a team drive in the member's catalogue (#1988). A member sees
// the team drives the server lists for the groups they hold, says who shares
// each, and is never shown a way to change a read-only one. With no `team` on
// the catalogue row the drive is today's.
import * as React from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { NR_FILES } from "../../../../wardyn/copy/new-run/files";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import { filesPreflight, MAIN_DRIVE_FACTS, SCENARIO_FILES_WORKSPACES } from "../../../../../lib/new-run-fixtures/scenarios/files";
import type { MeDrive, MeDriveCatalogue } from "../../../../../lib/api/me-drives";
import type { RunIntent } from "../../draft/run-intent";
import { NewRunDraftProvider, useNewRunDispatch, useNewRunDraft } from "../../new-run-route";
import { driveKind } from "./file-items";
import { FilesStep } from "./files-step";

const wfetchMock = vi.fn();
vi.mock("../../../../../lib/api/core", async (orig) => ({ ...(await orig<typeof import("../../../../../lib/api/core")>()), wfetch: (...a: unknown[]) => wfetchMock(...a) }));
const checks = vi.hoisted(() => ({ result: null as unknown }));
vi.mock("../../draft/use-intent-checks", () => ({
  useDraftIdentity: () => ({ principal: "alice", resolved: true, revision: 0, authGeneration: 0 }),
  useIntentChecks: (params: { body: Record<string, unknown> | null }) => ({ preflightResult: params.body === null ? null : checks.result }),
}));

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });
const base = { source: "team", enforcement: "none", legacy: false, owned: false } as const;
// A Docker volume is a managed (user) section for one person; for a group it is one shared folder.
const READ_ONLY: MeDrive = {
  ...base, id: "44444444-4444-4444-8444-444444444444", name: "platform-ref", section: "user", backend: "docker_volume",
  target: "/home/agent/shared/platform-ref", writable: false, team: true, shared_with: ["platform-team"],
};
const WRITABLE: MeDrive = {
  ...base, id: "55555555-5555-4555-8555-555555555555", name: "team-scratch", section: "shared", backend: "host_path",
  target: "/home/agent/shared/team-scratch", writable: true, team: true, shared_with: ["data-team", "qa"],
};
// The same group-allocated share without the team flag: today's row, no team words.
const PLAIN: MeDrive = { ...base, id: "66666666-6666-4666-8666-666666666666", name: "Finance archive", section: "shared", backend: "host_path", target: "/home/agent/shared/finance-archive", writable: true, team: false };
const BACKGROUND: RunIntent = { shape: "application", experience: "background", workload: { kind: "command", command: "make" } };

let catalogue: MeDriveCatalogue;
beforeEach(() => {
  catalogue = { drives: [READ_ONLY, WRITABLE, PLAIN], pool_checked: true };
  checks.result = filesPreflight(MAIN_DRIVE_FACTS.filesystem);
  wfetchMock.mockImplementation(async (path: string) => {
    if (path.startsWith("/workspaces")) return json(SCENARIO_FILES_WORKSPACES.member);
    if (path.startsWith("/me/drives")) return json(catalogue);
    if (path.startsWith("/setup/status")) return json({ runner: { driver: "", confinement_classes: [], kubernetes: false, computer_folders: false } });
    if (path.startsWith("/me/runners")) return json([]);
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
function show(intent: RunIntent = BACKGROUND) {
  render(
    <MemoryRouter>
      <NewRunDraftProvider owner="alice">
        <Seed intent={intent} />
        <FilesStep location={{ screen: "step", step: "files" }} go={() => {}} startFromScratch={() => {}} />
        <Drawn />
      </NewRunDraftProvider>
    </MemoryRouter>,
  );
}
const dialog = () => screen.getByRole("dialog");
async function openShared() {
  await userEvent.click(await screen.findByRole("button", { name: NR_FILES.ADD_ARIA }));
  await userEvent.click(within(dialog()).getByRole("button", { name: new RegExp(`^${NR_FILES.KIND_SHARED_DRIVE}`) }));
}
const option = async (name: string) => (await within(dialog()).findByRole("radio", { name: new RegExp(`^${name}`) })).closest("label")!;
const card = (name: string) => screen.getAllByTestId("nr-attach-card").find((c) => within(c).queryByText(name, { selector: "strong" }))!;

describe("a team drive in the Add shared drive dialog", () => {
  it("lists a Docker-volume team drive under shared drives, with Team, its access chip and who shares it", async () => {
    show();
    await openShared();
    expect(within(dialog()).getByRole("heading", { name: NR_FILES.ADD_SHARED_DRIVE_TITLE })).toBeInTheDocument();
    const ro = within(await option("platform-ref"));
    expect(ro.getByText(NR_FILES.SOURCE_TEAM)).toBeInTheDocument();
    expect(ro.getByText(NR_FILES.ACCESS_READ)).toBeInTheDocument();
    expect(ro.queryByText(NR_FILES.ACCESS_EDIT)).toBeNull();
    expect(ro.getByTitle("/home/agent/shared/platform-ref")).toBeInTheDocument();
    expect(ro.getByText(NR_FILES.SHARED_WITH("platform-team"))).toBeInTheDocument();
    // Read-only says nothing about what others see: the run cannot write there.
    expect(ro.queryByText(NR_FILES.TEAM_OTHERS_SEE)).toBeNull();
  });

  it("a writable team drive says Can edit and that the others in the group see what the run writes", async () => {
    show();
    await openShared();
    const rw = within(await option("team-scratch"));
    expect(rw.getByText(NR_FILES.ACCESS_EDIT)).toBeInTheDocument();
    expect(rw.getByText(NR_FILES.SHARED_WITH("data-team, qa"))).toBeInTheDocument();
    expect(rw.getByText(NR_FILES.TEAM_OTHERS_SEE)).toBeInTheDocument();
  });

  it("a shared drive without the team flag is today's row", async () => {
    show();
    await openShared();
    const plain = within(await option("Finance archive"));
    expect(plain.getByText(NR_FILES.FAMILY_HOST_FOLDER)).toBeInTheDocument();
    expect(plain.queryByText(/^Shared with/)).toBeNull();
    expect(plain.queryByText(NR_FILES.TEAM_OTHERS_SEE)).toBeNull();
  });

  it("a read-only team drive is added with Access locked at Read-only, and the card says who shares it", async () => {
    show();
    await openShared();
    await userEvent.click(within(await option("platform-ref")).getByRole("radio"));
    await userEvent.click(within(dialog()).getByRole("button", { name: new RegExp(`^${NR_SHARED.CONTINUE}`) }));
    // No select to choose Can edit from: the lock is the only control.
    expect(within(dialog()).queryByLabelText(NR_FILES.ACCESS_LABEL, { selector: "select" })).toBeNull();
    expect(within(dialog()).getByRole("button", { name: NR_FILES.LOCKED_ARIA })).toBeInTheDocument();
    await userEvent.click(within(dialog()).getByRole("button", { name: NR_FILES.ADD_CONFIRM }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());

    expect(drawn().drives).toEqual([{ drive_id: READ_ONLY.id }]);
    const c = within(card("platform-ref"));
    expect(c.getByText(NR_FILES.KIND_SHARED_DRIVE)).toBeInTheDocument();
    expect(c.getByText(NR_FILES.SHARED_WITH("platform-team"))).toBeInTheDocument();
    expect(c.getByText(NR_FILES.ACCESS_READ)).toBeInTheDocument();
    expect(c.queryByText(NR_FILES.ACCESS_EDIT)).toBeNull();
  });

  it("a writable team drive can be added with edit access, and its card says Can edit", async () => {
    show();
    await openShared();
    await userEvent.click(within(await option("team-scratch")).getByRole("radio"));
    await userEvent.click(within(dialog()).getByRole("button", { name: new RegExp(`^${NR_SHARED.CONTINUE}`) }));
    await userEvent.click(within(dialog()).getByRole("button", { name: NR_FILES.ADD_CONFIRM }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(within(card("team-scratch")).getByText(NR_FILES.ACCESS_EDIT)).toBeInTheDocument();
  });

  it("the person's top-ranked drive being a team drive is a shared drive card, not a user drive", async () => {
    catalogue = { drives: [{ ...READ_ONLY, legacy: true }], pool_checked: true };
    show({ ...BACKGROUND, drive: { enabled: true } });
    const c = within(await screen.findByText("platform-ref", { selector: "strong" }).then((e) => e.closest('[data-testid="nr-attach-card"]') as HTMLElement));
    expect(c.getByText(NR_FILES.KIND_SHARED_DRIVE)).toBeInTheDocument();
    expect(c.getByText(NR_FILES.SHARED_WITH("platform-team"))).toBeInTheDocument();
    expect(c.getByText(NR_FILES.ACCESS_READ)).toBeInTheDocument();
  });

  it("a Docker-volume team drive is not offered under user drives", async () => {
    show();
    await userEvent.click(await screen.findByRole("button", { name: NR_FILES.ADD_ARIA }));
    await userEvent.click(within(dialog()).getByRole("button", { name: new RegExp(`^${NR_FILES.KIND_USER_DRIVE}`) }));
    await within(dialog()).findByRole("heading", { name: NR_FILES.ADD_USER_DRIVE_TITLE });
    expect(within(dialog()).queryByRole("radio", { name: /^platform-ref/ })).toBeNull();
  });
});

describe("a team drive a person made, shared with their unit (#3459)", () => {
  const UNIT_RW: MeDrive = {
    ...base, id: "77777777-7777-4777-8777-777777777777", name: "design-assets", section: "user", backend: "docker_volume",
    target: "/home/agent/shared/design-assets", writable: true, team: true, shared_with_unit: true,
  };
  const UNIT_RO: MeDrive = { ...UNIT_RW, id: "88888888-8888-4888-8888-888888888888", name: "old-templates", target: "/home/agent/shared/old-templates", writable: false };

  it("a writable drive shared with a unit says unit, not group", async () => {
    catalogue = { drives: [UNIT_RW, UNIT_RO, WRITABLE], pool_checked: true };
    show();
    // A group's team drive keeps the group sentence, under shared drives.
    await openShared();
    expect(within(await option("team-scratch")).getByText(NR_FILES.TEAM_OTHERS_SEE)).toBeInTheDocument();
    expect(within(dialog()).queryByRole("radio", { name: /^design-assets/ })).toBeNull();
    await userEvent.click(within(dialog()).getByRole("button", { name: NR_SHARED.BACK }));
    // A unit's team drive lists under user drives.
    await userEvent.click(within(dialog()).getByRole("button", { name: new RegExp(`^${NR_FILES.KIND_USER_DRIVE}`) }));
    const rw = within(await option("design-assets"));
    expect(rw.getByText(NR_FILES.TEAM_OTHERS_SEE_UNIT)).toBeInTheDocument();
    expect(rw.queryByText(NR_FILES.TEAM_OTHERS_SEE)).toBeNull();
    // A read-only unit drive says neither, and reads Team and Read-only.
    const ro = within(await option("old-templates"));
    expect(ro.queryByText(NR_FILES.TEAM_OTHERS_SEE_UNIT)).toBeNull();
    expect(ro.queryByText(NR_FILES.TEAM_OTHERS_SEE)).toBeNull();
    expect(ro.getByText(NR_FILES.SOURCE_TEAM)).toBeInTheDocument();
    expect(ro.getByText(NR_FILES.ACCESS_READ)).toBeInTheDocument();
  });
});

describe("a person's own team drive whose unit was deleted (#3459)", () => {
  it("lists with user drives and says no one else sees what the run writes", async () => {
    const ORPHAN: MeDrive = {
      ...base, id: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", name: "old-templates", section: "user", backend: "docker_volume",
      target: "/home/agent/shared/old-templates", writable: true, team: true, owned: true, shared_with_unit: false,
    };
    catalogue = { drives: [ORPHAN, WRITABLE], pool_checked: true };
    show();
    await userEvent.click(await screen.findByRole("button", { name: NR_FILES.ADD_ARIA }));
    await userEvent.click(within(dialog()).getByRole("button", { name: new RegExp(`^${NR_FILES.KIND_USER_DRIVE}`) }));
    const row = within(await option("old-templates"));
    expect(row.queryByText(NR_FILES.TEAM_OTHERS_SEE)).toBeNull();
    expect(row.queryByText(NR_FILES.TEAM_OTHERS_SEE_UNIT)).toBeNull();
  });
});

// Where the Add dialog lists a drive: one row per value of the predicate.
describe("driveKind", () => {
  const MANAGED: MeDrive = { ...PLAIN, id: "99999999-9999-4999-8999-999999999999", name: "scratch", section: "user", backend: "docker_volume", team: false };
  it.each<[string, MeDrive, "sharedDrive" | "userDrive"]>([
    ["a group's team drive is shared", READ_ONLY, "sharedDrive"],
    ["a unit's team drive is a user drive", { ...READ_ONLY, shared_with: undefined, shared_with_unit: true }, "userDrive"],
    ["a managed drive that is not a team drive is a user drive", MANAGED, "userDrive"],
    ["the owner's team drive whose unit was deleted is a user drive", { ...READ_ONLY, shared_with: undefined, owned: true, writable: true }, "userDrive"],
    ["a drive in the shared section is shared", PLAIN, "sharedDrive"],
  ])("%s", (_, d, kind) => {
    expect(driveKind(d)).toBe(kind);
  });
});

