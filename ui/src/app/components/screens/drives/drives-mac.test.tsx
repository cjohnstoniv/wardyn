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
import { DRIVE_REFUSAL } from "../../../lib/drive-refusals";
import type { MemberDriveAdminItem, UserDriveListItem, UserDrivesSnapshot } from "../../../lib/api/drives";
import { aheadByHours } from "../../../lib/test-clock";
import { DRIVES, PEOPLE } from "../../../lib/user-drives-copy";
import { MAC_DRIVES } from "../../wardyn/copy/mac-drives";
import { MEMBER_DRIVES as T } from "../../wardyn/copy/member-drives";
import { NR_FILES } from "../../wardyn/copy/new-run/files";
import { NR_SHARED } from "../../wardyn/copy/new-run/shared";
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
    // A disk on a Mac: the confirm says, before the click, that an offline or unregistered Mac keeps the disk.
    expect(dialog.getByText(T.DELETE_CONFIRM_MAC("Investigation files"))).toBeInTheDocument();
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

  // A disk drive whose Mac is no longer registered loses only its record; the row goes, the disk stays on that Mac,
  // which the runner list no longer names. Catches a confirm that closes as if the files were deleted.
  it("a delete that removed only the record says the disk stays on its Mac", async () => {
    deleteMemberDrive.mockResolvedValue({ outcome: "record_only" });
    listRunners.mockResolvedValue([{ id: "r-kofi", name: "kofi-mbp" }]);
    render(<DrivesScreen />);
    const s = await section();
    await s.findByText("Investigation files");
    await waitFor(() => expect(listRunners).toHaveBeenCalled());
    await userEvent.click(s.getByRole("button", { name: `${DRIVES.DELETE} Investigation files` }));
    listMemberDrives.mockResolvedValue({ drives: [BUSY, mine({})], total: 2 });
    await userEvent.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: DRIVES.DELETE }));
    const dialog = within(screen.getByRole("alertdialog"));
    expect(await dialog.findByText(T.RECORD_ONLY_UNNAMED)).toBeInTheDocument();
    expect(dialog.queryByText(T.DELETE_CONFIRM_MAC("Investigation files"))).toBeNull();
    expect(dialog.queryByRole("button", { name: DRIVES.DELETE })).toBeNull();
    await waitFor(() => expect(s.queryByText("Investigation files")).toBeNull());
    await userEvent.click(dialog.getByRole("button", { name: NR_SHARED.CLOSE_DIALOG }));
    expect(screen.queryByRole("alertdialog")).toBeNull();
  });

  // The last of people's drives, record only: the list is then empty, but the sentence stays until it is closed.
  // Catches a section that unmounts the confirm with the answer still unread.
  it("a record-only delete of the last drive still says the disk stays on its Mac", async () => {
    deleteMemberDrive.mockResolvedValue({ outcome: "record_only" });
    render(<DrivesScreen />);
    const s = await section();
    await s.findByText("mac-studio-07");
    await userEvent.click(s.getByRole("button", { name: `${DRIVES.DELETE} Investigation files` }));
    listMemberDrives.mockResolvedValue({ drives: [], total: 0 });
    await userEvent.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: DRIVES.DELETE }));
    await waitFor(() => expect(screen.queryByText("Investigation files")).toBeNull());
    expect(screen.getByText(DRIVE_REFUSAL.MEMBER_RECORD_ONLY("mac-studio-07"))).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: NR_SHARED.CLOSE_DIALOG }));
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(screen.queryByTestId("member-drives")).toBeNull();
  });

  // Catches a confirm that can be closed while its delete is in flight, after which the delete's answer would land on
  // whatever confirm is open by then.
  it("the confirm cannot be closed while its delete is in flight", async () => {
    let answer: (v: unknown) => void = () => {};
    deleteMemberDrive.mockReturnValue(new Promise((r) => { answer = r; }));
    render(<DrivesScreen />);
    const s = await section();
    await userEvent.click(await s.findByRole("button", { name: `${DRIVES.DELETE} Notes` }));
    const dialog = within(await screen.findByRole("alertdialog"));
    // A drive that is not on a Mac keeps the plain confirm.
    expect(dialog.getByText(NR_FILES.DELETE_CONFIRM("Notes"))).toBeInTheDocument();
    await userEvent.click(dialog.getByRole("button", { name: DRIVES.DELETE }));
    expect(dialog.getByRole("button", { name: PEOPLE.CANCEL })).toBeDisabled();
    await userEvent.keyboard("{Escape}");
    expect(screen.getByRole("alertdialog")).toBeInTheDocument();
    answer({ outcome: "deleted" });
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
  });
});
