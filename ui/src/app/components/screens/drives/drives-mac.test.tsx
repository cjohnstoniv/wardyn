/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The admin drive list with a disk kept on a Mac (board J4): the Storage column, and the Mac column that names
// the runner. Sentences are read from the copy modules.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import type { UserDriveListItem, UserDrivesSnapshot } from "../../../lib/api/drives";
import { aheadByHours } from "../../../lib/test-clock";
import { DRIVES } from "../../../lib/user-drives-copy";
import { MAC_DRIVES } from "../../wardyn/copy/mac-drives";
import { DrivesScreen } from "./drives-screen";

const getDrives = vi.fn();
vi.mock("../../../lib/api/drives", async (orig) => ({
  ...(await orig<typeof import("../../../lib/api/drives")>()),
  drives: { getDrives: () => getDrives() },
}));
const listRunners = vi.fn();
vi.mock("../../../lib/api/runners", () => ({ runners: { list: (...a: unknown[]) => listRunners(...a) } }));
vi.mock("../../../lib/api/providers", () => ({ providers: { getWorkspaceProviders: () => Promise.resolve({ providers: {} }) } }));

const row = (over: Partial<UserDriveListItem>): UserDriveListItem => ({
  id: "d1", name: "Corporate homes", backend: "k8s_pvc", home_template: "hash", reclaim: "retain", grant_count: 0,
  created_at: aheadByHours(-1), updated_at: aheadByHours(-1), ...over,
});
const snap = (drives: UserDriveListItem[]): UserDrivesSnapshot => ({ drives, grants: [], host_roots_configured: false, runner_target: "docker", disabled: false });

beforeEach(() => {
  getDrives.mockReset();
  listRunners.mockReset();
  listRunners.mockResolvedValue([{ id: "r-mac", name: "mac-studio-07" }]);
});
afterEach(cleanup);

describe("admin drive list, a disk kept on a Mac", () => {
  it("the Storage column says Mac disk, and the Mac column names the runner (MAC-D09, D15, D16)", async () => {
    getDrives.mockResolvedValue(snap([row({ id: "d2", name: "Investigation files", backend: "mac_disk", home_runner_id: "r-mac", size_mib: 65536 }), row({ id: "d1" })]));
    render(<DrivesScreen />);
    const disk = (await screen.findByText("Investigation files")).closest("tr")!;
    const headers = screen.getAllByRole("columnheader").map((h) => h.textContent);
    expect(headers).toContain(MAC_DRIVES.D15);
    expect(headers).toContain(MAC_DRIVES.D09);
    expect(headers).not.toContain(DRIVES.COL_BACKEND);
    expect(within(disk).getByText(MAC_DRIVES.D16)).toBeInTheDocument();
    expect(await within(disk).findByText("mac-studio-07")).toBeInTheDocument();
    // A drive kept elsewhere has nothing in the Mac column, and keeps its wire name.
    const other = screen.getByText("Corporate homes").closest("tr")!;
    expect(within(other).getByTestId("drives-mac")).toBeEmptyDOMElement();
    expect(within(other).getByText("k8s_pvc")).toBeInTheDocument();
  });
  it("no Mac column, and no runner read, while no drive is kept on a Mac", async () => {
    getDrives.mockResolvedValue(snap([row({})]));
    render(<DrivesScreen />);
    await screen.findByText("Corporate homes");
    expect(screen.queryByRole("columnheader", { name: MAC_DRIVES.D09 })).toBeNull();
    expect(listRunners).not.toHaveBeenCalled();
  });
});
