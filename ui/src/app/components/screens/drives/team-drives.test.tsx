/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Team drives on the admin Drives screen (#1988), behind
// storage.user_drive.team_drives. What is pinned is the security shape: a team
// drive is read-only unless the admin ticks Members can write, the one toggle
// is written to the drive AND its allocations (never claimed from the drive
// alone), allocations are groups only and sit beside the group's other drives,
// and with the switch off the screen is today's.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { act, render, screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn() } }));

const getDrivesMock = vi.fn();
const createDriveMock = vi.fn();
const updateDriveMock = vi.fn();
const upsertGrantMock = vi.fn();
const deleteGrantMock = vi.fn();
vi.mock("../../../lib/api/drives", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/drives")>("../../../lib/api/drives");
  return {
    ...actual,
    drives: {
      getDrives: () => getDrivesMock(),
      createDrive: (...a: unknown[]) => createDriveMock(...a),
      updateDrive: (...a: unknown[]) => updateDriveMock(...a),
      upsertGrant: (...a: unknown[]) => upsertGrantMock(...a),
      deleteGrant: (...a: unknown[]) => deleteGrantMock(...a),
      deleteDrive: vi.fn(),
      previewDrive: vi.fn().mockResolvedValue({}),
    },
  };
});

const getProvidersMock = vi.fn();
vi.mock("../../../lib/api/providers", () => ({ providers: { getWorkspaceProviders: () => getProvidersMock() } }));
// No directory configured: the group field is the plain input it falls back to.
vi.mock("../../../lib/api/directory", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/directory")>("../../../lib/api/directory");
  return { ...actual, directory: { search: vi.fn().mockResolvedValue(null) } };
});

import { HttpError } from "../../../lib/api/core";
import type { UserDriveGrant, UserDriveListItem, UserDrivesSnapshot } from "../../../lib/api/drives";
import { DRIVES, PERM } from "../../../lib/user-drives-copy";
import { aheadByHours } from "../../../lib/test-clock";
import { DrivesScreen } from "./drives-screen";

const when = aheadByHours(-1);

function drive(over: Partial<UserDriveListItem> = {}): UserDriveListItem {
  return {
    id: "t1",
    name: "platform-ref",
    backend: "docker_volume",
    home_template: "hash",
    size_mib: 10240,
    reclaim: "retain",
    grant_count: 1,
    team: true,
    created_at: when,
    updated_at: when,
    ...over,
  };
}

function grant(over: Partial<UserDriveGrant> = {}): UserDriveGrant {
  return {
    id: "g1",
    subject_type: "group",
    subject: "platform-team",
    drive_id: "t1",
    priority: 0,
    enabled: true,
    created_at: when,
    ...over,
  };
}

function snapshot(over: Partial<UserDrivesSnapshot> = {}): UserDrivesSnapshot {
  return { drives: [drive()], grants: [grant()], host_roots_configured: true, runner_target: "docker", disabled: false, ...over };
}

async function renderScreen(snap: UserDrivesSnapshot, teamDrivesOn = true) {
  getDrivesMock.mockResolvedValue(snap);
  getProvidersMock.mockResolvedValue({ providers: { storage: { user_drive: { team_drives: teamDrivesOn || undefined } } }, etag: null });
  render(<DrivesScreen />);
  await screen.findAllByRole("table");
  // The switch is a second read: let it land before the test looks.
  await waitFor(() => expect(getProvidersMock).toHaveBeenCalled());
  await act(async () => {});
}

// The chips and the Allocate button repeat names the allocations block below also carries.
const editor = () => within(screen.getByTestId("drives-drive-editor"));
const table = () => within(screen.getAllByRole("table")[0]);
const openEdit = async (name: string) => userEvent.click(await screen.findByRole("button", { name: `${DRIVES.EDIT} ${name}` }));

beforeEach(() => {
  for (const m of [getDrivesMock, createDriveMock, updateDriveMock, upsertGrantMock, deleteGrantMock, getProvidersMock]) m.mockReset();
  createDriveMock.mockImplementation(async (input: object) => ({ id: "new1", ...input }));
  updateDriveMock.mockImplementation(async (id: string, input: object) => ({ id, ...input }));
  upsertGrantMock.mockResolvedValue({ grant: grant(), replaced: false });
  deleteGrantMock.mockResolvedValue(undefined);
});

describe("Drives list, team drive rows", () => {
  it("a team drive carries the Team drive chip, One host only, Kept (team drive) and its group names", async () => {
    await renderScreen(snapshot({ drives: [drive({ single_host: true, grant_count: 2 })], grants: [grant(), grant({ id: "g2", subject: "qa" })] }));
    expect(table().getByText(DRIVES.TEAM_CHIP)).toBeInTheDocument();
    expect(table().getByText(DRIVES.TEAM_ONE_HOST_CHIP)).toBeInTheDocument();
    expect(table().getByText(DRIVES.TEAM_RECLAIM_CELL)).toBeInTheDocument();
    expect(table().getByText("platform-team, qa")).toBeInTheDocument();
  });

  it("Mode says Writable only where the drive and every allocation let members write", async () => {
    const rw = drive({ writable: true });
    // The drive is writable but its allocation is not: the mount is read-only, so the row says so.
    await renderScreen(snapshot({ drives: [rw], grants: [grant()] }));
    expect(table().getByText(DRIVES.MODE_RO)).toBeInTheDocument();
    expect(table().queryByText(DRIVES.MODE_RW)).toBeNull();
  });

  it("Mode is Writable when the drive and its allocation both say so", async () => {
    await renderScreen(snapshot({ drives: [drive({ writable: true })], grants: [grant({ members_can_write: true })] }));
    expect(table().getByText(DRIVES.MODE_RW)).toBeInTheDocument();
  });

  it("a page of allocations shorter than the count cannot prove write, so the row stays read-only", async () => {
    await renderScreen(snapshot({ drives: [drive({ writable: true, grant_count: 3 })], grants: [grant({ members_can_write: true })] }));
    expect(table().getByText(DRIVES.MODE_RO)).toBeInTheDocument();
    expect(table().getByText(DRIVES.ALLOCATED_COUNT(3))).toBeInTheDocument();
  });

  it("switch off: the same drive is today's row, with no chip and no team words", async () => {
    await renderScreen(snapshot({ drives: [drive({ single_host: true, writable: true })] }), false);
    expect(screen.queryByText(DRIVES.TEAM_CHIP)).toBeNull();
    expect(screen.queryByText(DRIVES.TEAM_ONE_HOST_CHIP)).toBeNull();
    expect(screen.queryByText(DRIVES.TEAM_RECLAIM_CELL)).toBeNull();
    expect(table().getByText(DRIVES.ALLOCATED_COUNT(1))).toBeInTheDocument();
    await openEdit("platform-ref");
    expect(screen.queryByRole("switch", { name: DRIVES.FIELD_TEAM })).toBeNull();
    expect(screen.getByRole("switch", { name: DRIVES.FIELD_WRITABLE })).toBeInTheDocument();
  });
});

describe("Drive editor, Team drive", () => {
  it("Team drive replaces Writable with the one Members can write toggle, off, and drops the per-person fields", async () => {
    await renderScreen(snapshot({ drives: [drive({ team: false, writable: true, grant_count: 0 })], grants: [] }));
    await openEdit("platform-ref");
    const team = screen.getByRole("switch", { name: DRIVES.FIELD_TEAM });
    expect(team).toHaveAttribute("aria-checked", "false");
    expect(screen.getByRole("switch", { name: DRIVES.FIELD_WRITABLE })).toHaveAttribute("aria-checked", "true");

    await userEvent.click(team);
    // Ticking Team drive starts the write toggle off even though the drive was writable.
    expect(screen.getByRole("switch", { name: DRIVES.FIELD_TEAM_WRITE })).toHaveAttribute("aria-checked", "false");
    expect(screen.queryByRole("switch", { name: DRIVES.FIELD_WRITABLE })).toBeNull();
    expect(screen.getByText(DRIVES.TEAM_NO_HOME)).toBeInTheDocument();
    expect(screen.getByText(DRIVES.TEAM_RECLAIM_KEPT)).toBeInTheDocument();
    expect(screen.getByText(DRIVES.TEAM_SIZE_HINT)).toBeInTheDocument();
    expect(screen.queryByText(DRIVES.HOME_HINT)).toBeNull();
    expect(screen.queryByRole("radio", { name: DRIVES.RECLAIM_DELETE })).toBeNull();
    expect(screen.getByText(DRIVES.TEAM_ALLOC_HINT)).toBeInTheDocument();
  });

  it("the Docker-volume notes: one host only, and the read-only team drive that stays empty", async () => {
    await renderScreen(snapshot());
    await openEdit("platform-ref");
    expect(screen.getByText(DRIVES.TEAM_ONE_HOST)).toBeInTheDocument();
    expect(screen.getByText(DRIVES.TEAM_EMPTY_READ_ONLY)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("switch", { name: DRIVES.FIELD_TEAM_WRITE }));
    expect(screen.getByText(DRIVES.TEAM_ONE_HOST)).toBeInTheDocument();
    expect(screen.queryByText(DRIVES.TEAM_EMPTY_READ_ONLY)).toBeNull();
  });

  it("a pre-created claim says ReadWriteMany and names the team in its option", async () => {
    await renderScreen(
      snapshot({ drives: [drive({ backend: "k8s_pvc_static" })], runner_target: "k8s" }),
    );
    await openEdit("platform-ref");
    expect(screen.getByText(DRIVES.TEAM_CLAIM_NOTE)).toBeInTheDocument();
    expect(screen.getByText(DRIVES.BACKEND_K8S_PVC_STATIC_TEAM)).toBeInTheDocument();
    expect(screen.queryByText(DRIVES.BACKEND_K8S_PVC_STATIC)).toBeNull();
  });

  it("an existing team drive opens with Members can write off when its allocation lacks the bit", async () => {
    await renderScreen(snapshot({ drives: [drive({ writable: true })], grants: [grant()] }));
    await openEdit("platform-ref");
    expect(screen.getByRole("switch", { name: DRIVES.FIELD_TEAM_WRITE })).toHaveAttribute("aria-checked", "false");
    expect(within(screen.getByTestId("drives-team-groups")).getByText("platform-team")).toBeInTheDocument();
  });

  it("saves a new team drive read-only and allocates the chosen group beside its other drives", async () => {
    await renderScreen(snapshot({ drives: [drive({ id: "other", team: false, name: "home" })], grants: [] }));
    await userEvent.click(await screen.findByRole("button", { name: DRIVES.NEW_CTA }));
    await userEvent.type(screen.getByLabelText(DRIVES.FIELD_NAME), "team-scratch");
    await userEvent.click(screen.getByRole("switch", { name: DRIVES.FIELD_TEAM }));
    expect(screen.getByRole("textbox", { name: PERM.SUBJECT_GROUP })).toHaveAttribute("placeholder", DRIVES.TEAM_GROUP_PLACEHOLDER);
    await userEvent.type(screen.getByRole("textbox", { name: PERM.SUBJECT_GROUP }), "data-team");
    await userEvent.click(editor().getByRole("button", { name: DRIVES.ADD_CTA }));
    await userEvent.click(screen.getByRole("button", { name: DRIVES.SAVE_CTA }));

    await waitFor(() => expect(upsertGrantMock).toHaveBeenCalledTimes(1));
    expect(createDriveMock).toHaveBeenCalledWith(expect.objectContaining({ name: "team-scratch", team: true, writable: false, reclaim: "retain" }));
    expect(upsertGrantMock).toHaveBeenCalledWith(
      expect.objectContaining({ subject_type: "group", subject: "data-team", drive_id: "new1", additional: true, members_can_write: false }),
    );
    // The drive is written first, so an allocation never names a drive that is not there yet.
    expect(createDriveMock.mock.invocationCallOrder[0]).toBeLessThan(upsertGrantMock.mock.invocationCallOrder[0]);
  });

  it("a team drive is kept when a person leaves, whatever the drive said before", async () => {
    await renderScreen(snapshot({ drives: [drive({ team: false, reclaim: "delete", grant_count: 0 })], grants: [] }));
    await openEdit("platform-ref");
    await userEvent.click(screen.getByRole("switch", { name: DRIVES.FIELD_TEAM }));
    await userEvent.click(screen.getByRole("button", { name: DRIVES.SAVE_CTA }));
    await waitFor(() => expect(updateDriveMock).toHaveBeenCalledTimes(1));
    expect(updateDriveMock).toHaveBeenCalledWith("t1", expect.objectContaining({ team: true, reclaim: "retain" }), false);
  });

  it("Members can write is written to the drive and to every allocation, drive first", async () => {
    await renderScreen(snapshot({ drives: [drive({ grant_count: 2 })], grants: [grant({ priority: 4 }), grant({ id: "g2", subject: "qa", members_can_write: true, writable_override: false })] }));
    await openEdit("platform-ref");
    await userEvent.click(screen.getByRole("switch", { name: DRIVES.FIELD_TEAM_WRITE }));
    await userEvent.click(screen.getByRole("button", { name: DRIVES.SAVE_CTA }));

    await waitFor(() => expect(upsertGrantMock).toHaveBeenCalledTimes(1));
    expect(updateDriveMock).toHaveBeenCalledWith("t1", expect.objectContaining({ team: true, writable: true }), false);
    // Only the allocation that did not carry the bit is re-sent, with its priority kept.
    expect(upsertGrantMock).toHaveBeenCalledWith(
      expect.objectContaining({ subject: "platform-team", priority: 4, additional: true, members_can_write: true }),
    );
    expect(updateDriveMock.mock.invocationCallOrder[0]).toBeLessThan(upsertGrantMock.mock.invocationCallOrder[0]);
  });

  it("turning Members can write off takes the bit off the allocations", async () => {
    await renderScreen(snapshot({ drives: [drive({ writable: true })], grants: [grant({ members_can_write: true })] }));
    await openEdit("platform-ref");
    const toggle = screen.getByRole("switch", { name: DRIVES.FIELD_TEAM_WRITE });
    expect(toggle).toHaveAttribute("aria-checked", "true");
    await userEvent.click(toggle);
    await userEvent.click(screen.getByRole("button", { name: DRIVES.SAVE_CTA }));
    await waitFor(() => expect(upsertGrantMock).toHaveBeenCalledTimes(1));
    expect(updateDriveMock).toHaveBeenCalledWith("t1", expect.objectContaining({ writable: false }), false);
    expect(upsertGrantMock).toHaveBeenCalledWith(expect.objectContaining({ members_can_write: false }));
  });

  it("removing a group chip unallocates it on save", async () => {
    await renderScreen(snapshot());
    await openEdit("platform-ref");
    await userEvent.click(editor().getByRole("button", { name: `${PERM.REMOVE} platform-team` }));
    await userEvent.click(screen.getByRole("button", { name: DRIVES.SAVE_CTA }));
    await waitFor(() => expect(deleteGrantMock).toHaveBeenCalledWith("g1"));
    expect(upsertGrantMock).not.toHaveBeenCalled();
  });

  it("changing Team drive on an allocated drive meets the server's refusal, not the re-home confirm", async () => {
    const refusal = "team cannot change while the drive is allocated to 1 subject: remove its allocations first";
    updateDriveMock.mockRejectedValue(new HttpError(409, refusal));
    await renderScreen(snapshot({ drives: [drive({ team: false })], grants: [grant()] }));
    await openEdit("platform-ref");
    await userEvent.click(screen.getByRole("switch", { name: DRIVES.FIELD_TEAM }));
    await userEvent.click(screen.getByRole("button", { name: DRIVES.SAVE_CTA }));
    expect(await screen.findByText(refusal)).toBeInTheDocument();
    expect(screen.getByText(DRIVES.SAVE_REFUSED_TITLE)).toBeInTheDocument();
    expect(screen.queryByText(DRIVES.REHOME_TITLE)).toBeNull();
  });

  it("the allocation form sends a team drive's write bit and allocates beside the group's others", async () => {
    await renderScreen(snapshot({ drives: [drive({ writable: true })], grants: [] }));
    await userEvent.click(await screen.findByRole("combobox", { name: DRIVES.FIELD_DRIVE }));
    await userEvent.click(await screen.findByRole("option", { name: "platform-ref" }));
    await userEvent.type(screen.getByRole("textbox", { name: PERM.FIELD_WHO }), "qa");
    await userEvent.click(screen.getByRole("button", { name: DRIVES.ADD_CTA }));
    await waitFor(() => expect(upsertGrantMock).toHaveBeenCalledTimes(1));
    expect(upsertGrantMock).toHaveBeenCalledWith(
      expect.objectContaining({ subject_type: "group", subject: "qa", drive_id: "t1", additional: true, members_can_write: true }),
    );
  });
});
