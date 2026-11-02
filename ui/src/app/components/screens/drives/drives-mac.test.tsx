/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The admin Drives screen's list of people's drives (board J4, "Admin view → User drives"), a disk kept on a Mac
// among them. The registry list above it never holds one, and has no Mac column. Sentences are read from the copy modules.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { HttpError } from "../../../lib/api/core";
import type { MemberDriveAdminItem, UserDriveListItem, UserDrivesSnapshot } from "../../../lib/api/drives";
import { aheadByHours } from "../../../lib/test-clock";
import { DRIVES } from "../../../lib/user-drives-copy";
import { MAC_DRIVES } from "../../wardyn/copy/mac-drives";
import { MEMBER_DRIVES as T } from "../../wardyn/copy/member-drives";
import { NR_FILES } from "../../wardyn/copy/new-run/files";
import { OperatorProvider } from "../../wardyn/operator-context";
import { DrivesScreen } from "./drives-screen";

const getDrives = vi.fn();
const listMemberDrives = vi.fn();
const deleteMemberDrive = vi.fn();
vi.mock("../../../lib/api/drives", async (orig) => ({
  ...(await orig<typeof import("../../../lib/api/drives")>()),
  drives: {
    getDrives: () => getDrives(),
    listMemberDrives: (...a: unknown[]) => listMemberDrives(...a),
    deleteMemberDrive: (...a: unknown[]) => deleteMemberDrive(...a),
  },
}));
const listRunners = vi.fn();
vi.mock("../../../lib/api/runners", () => ({ runners: { list: (...a: unknown[]) => listRunners(...a) } }));
vi.mock("../../../lib/api/providers", () => ({ providers: { getWorkspaceProviders: () => Promise.resolve({ providers: {} }) } }));

const row = (over: Partial<UserDriveListItem>): UserDriveListItem => ({
  id: "d1", name: "Corporate homes", backend: "k8s_pvc", home_template: "hash", reclaim: "retain", grant_count: 0,
  created_at: aheadByHours(-1), updated_at: aheadByHours(-1), ...over,
});
const snap = (drives: UserDriveListItem[]): UserDrivesSnapshot => ({ drives, grants: [], host_roots_configured: false, runner_target: "docker", disabled: false });
const mine = (over: Partial<MemberDriveAdminItem>): MemberDriveAdminItem => ({
  id: "m1", name: "Notes", owner: "lena@example.com", backend: "docker_volume", object: "vol", created_at: aheadByHours(-1), deleting: false, ...over,
});
const DISK = mine({ id: "m2", name: "Investigation files", owner: "dana@example.com", backend: "mac_disk", home_runner_id: "r-mac", size_mib: 65536 });
const BUSY = mine({ id: "m3", name: "Build cache", owner: "kofi@example.com", backend: "mac_disk", home_runner_id: "r-kofi", size_mib: 65536, deleting: true });

beforeEach(() => {
  getDrives.mockReset().mockResolvedValue(snap([row({})]));
  listMemberDrives.mockReset().mockResolvedValue({ drives: [DISK, BUSY, mine({})], total: 3 });
  deleteMemberDrive.mockReset().mockResolvedValue({ outcome: "deleted" });
  listRunners.mockReset().mockResolvedValue([{ id: "r-mac", name: "mac-studio-07" }, { id: "r-kofi", name: "kofi-mbp" }]);
});
afterEach(cleanup);

const section = async () => within(await screen.findByTestId("member-drives"));

describe("the registry list, which never holds a disk on a Mac", () => {
  it("keeps its Backend column and has no Mac column or runner read", async () => {
    render(<DrivesScreen />);
    await screen.findByText("Corporate homes");
    const heads = within(screen.getAllByRole("table")[0]).getAllByRole("columnheader").map((h) => h.textContent);
    expect(heads).toContain(DRIVES.COL_BACKEND);
    expect(heads).not.toContain(MAC_DRIVES.D09);
  });
});

describe("people's drives", () => {
  it("lists every drive with its owner, storage, Mac, size and state (MAC-D09, D15, D16)", async () => {
    render(<DrivesScreen />);
    const s = await section();
    await s.findByText("Investigation files");
    const heads = s.getAllByRole("columnheader").map((h) => h.textContent);
    expect(heads).toEqual(expect.arrayContaining([MAC_DRIVES.D15, MAC_DRIVES.D09, DRIVES.COL_SIZE, "Drive", "Owner", "State"]));

    const disk = s.getByText("Investigation files").closest("tr")!;
    expect(within(disk).getByText("dana@example.com")).toBeInTheDocument();
    expect(within(disk).getByText(MAC_DRIVES.D16)).toBeInTheDocument();
    expect(await within(disk).findByText("mac-studio-07")).toBeInTheDocument();
    expect(within(disk).getByText("64 GiB")).toBeInTheDocument();
    expect(within(disk).getByText(T.STATE_READY)).toBeInTheDocument();

    // A disk whose Mac is offline stays Deleting until it returns.
    expect(within(s.getByText("Build cache").closest("tr")!).getByText(T.STATE_DELETING)).toBeInTheDocument();

    // A Docker volume keeps today's name, and has no Mac and no size to show.
    const docker = s.getByText("Notes").closest("tr")!;
    expect(within(docker).getByText(NR_FILES.FAMILY_DOCKER_VOLUME)).toBeInTheDocument();
    expect(within(docker).getByTestId("member-drive-mac")).toHaveTextContent(T.NONE);
    expect(within(docker).getAllByText(T.NONE)).toHaveLength(2);
  });

  it("reads no runners while no drive is kept on a Mac", async () => {
    listMemberDrives.mockResolvedValue({ drives: [mine({})], total: 1 });
    render(<DrivesScreen />);
    await (await section()).findByText("Notes");
    expect(listRunners).not.toHaveBeenCalled();
  });

  it("is not there while no one has made a drive, and a failed read says so with a retry", async () => {
    listMemberDrives.mockResolvedValue({ drives: [], total: 0 });
    render(<DrivesScreen />);
    await screen.findByText("Corporate homes");
    await waitFor(() => expect(listMemberDrives).toHaveBeenCalled());
    expect(screen.queryByTestId("member-drives")).toBeNull();
    cleanup();
    listMemberDrives.mockRejectedValueOnce(new Error("down")).mockResolvedValueOnce({ drives: [mine({})], total: 1 });
    render(<DrivesScreen />);
    const s = await section();
    await userEvent.click(await s.findByRole("button", { name: "Retry" }));
    expect(await s.findByText("Notes")).toBeInTheDocument();
  });

  it("a security admin, whose read is a 403, sees no section and no error", async () => {
    listMemberDrives.mockRejectedValue(new HttpError(403, "forbidden"));
    render(<OperatorProvider operator securityOperator><DrivesScreen /></OperatorProvider>);
    await screen.findByText("Corporate homes");
    await waitFor(() => expect(listMemberDrives).toHaveBeenCalled());
    expect(screen.queryByTestId("member-drives")).toBeNull();
    expect(screen.queryByText(DRIVES.FETCH_FAILED_TITLE)).toBeNull();
  });

  it("a caller who is not the super admin makes no read of it at all", async () => {
    render(<OperatorProvider operator={false} securityOperator><DrivesScreen /></OperatorProvider>);
    await screen.findByText("Corporate homes");
    expect(listMemberDrives).not.toHaveBeenCalled();
    expect(screen.queryByTestId("member-drives")).toBeNull();
  });

  it("pages by the server's total: the first page, then more from where it ended", async () => {
    listMemberDrives.mockResolvedValueOnce({ drives: [DISK, BUSY], total: 3 }).mockResolvedValueOnce({ drives: [mine({})], total: 3 });
    render(<DrivesScreen />);
    const s = await section();
    await s.findByText("Investigation files");
    expect(listMemberDrives).toHaveBeenLastCalledWith(0);
    expect(s.getByText(T.SHOWING(2, 3))).toBeInTheDocument();
    await userEvent.click(s.getByRole("button", { name: T.SHOW_MORE }));
    expect(await s.findByText("Notes")).toBeInTheDocument();
    expect(listMemberDrives).toHaveBeenLastCalledWith(2);
    expect(s.queryByRole("button", { name: T.SHOW_MORE })).toBeNull();
  });

  it("deletes through the confirm, then reads the list again", async () => {
    render(<DrivesScreen />);
    const s = await section();
    await userEvent.click(await s.findByRole("button", { name: `${DRIVES.DELETE} Investigation files` }));
    const dialog = within(await screen.findByRole("alertdialog"));
    expect(dialog.getByText(NR_FILES.DELETE_CONFIRM("Investigation files"))).toBeInTheDocument();
    listMemberDrives.mockResolvedValue({ drives: [BUSY, mine({})], total: 2 });
    await userEvent.click(dialog.getByRole("button", { name: DRIVES.DELETE }));
    await waitFor(() => expect(deleteMemberDrive).toHaveBeenCalledWith("m2"));
    await waitFor(() => expect(s.queryByText("Investigation files")).toBeNull());
    expect(screen.queryByRole("alertdialog")).toBeNull();
  });

  it("a refused delete keeps the confirm up with the server's sentence", async () => {
    deleteMemberDrive.mockRejectedValue(new HttpError(409, "delete refused: a run of the drive's owner is starting"));
    render(<DrivesScreen />);
    const s = await section();
    await userEvent.click(await s.findByRole("button", { name: `${DRIVES.DELETE} Notes` }));
    await userEvent.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: DRIVES.DELETE }));
    expect(await screen.findByRole("alert")).toHaveTextContent("delete refused: a run of the drive's owner is starting");
    expect(screen.getByRole("alertdialog")).toBeInTheDocument();
  });
});
