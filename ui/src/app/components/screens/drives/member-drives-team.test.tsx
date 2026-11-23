/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A person's own team drive on the admin Drives screen (#3459): its chips, its unit, the "Members can write" switch
// and its confirm, and the delete confirm that says the unit loses the folder too.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { MemberDriveAdminItem } from "../../../lib/api/drives";
import { HttpError } from "../../../lib/api/core";
import { aheadByHours } from "../../../lib/test-clock";
import { DRIVES, PEOPLE } from "../../../lib/user-drives-copy";
import { MEMBER_DRIVES as T } from "../../wardyn/copy/member-drives";
import { NR_FILES } from "../../wardyn/copy/new-run/files";
import { MemberDrives } from "./member-drives";

const listMemberDrives = vi.fn();
const setMemberTeamDriveWrite = vi.fn();
vi.mock("../../../lib/api/drives", async (orig) => ({
  ...(await orig<typeof import("../../../lib/api/drives")>()),
  drives: {
    listMemberDrives: (...a: unknown[]) => listMemberDrives(...a),
    deleteMemberDrive: () => Promise.resolve({ outcome: "deleted" }),
    setMemberTeamDriveWrite: (...a: unknown[]) => setMemberTeamDriveWrite(...a),
  },
}));
vi.mock("../../../lib/api/runners", () => ({ runners: { list: () => Promise.resolve([]) } }));

const UNIT = "0b7d5c1e-2f3a-4b5c-8d9e-0f1a2b3c4d5e";
const base = { owner: "lena@example.com", backend: "docker_volume", object: "vol", created_at: aheadByHours(-1), deleting: false } as const;
const TEAM: MemberDriveAdminItem = { ...base, id: "m1", name: "design-assets", team: true, team_unit_id: UNIT, team_unit_name: "Platform", members_can_write: false };
const TEAM_ON: MemberDriveAdminItem = { ...TEAM, id: "m2", name: "build-cache", members_can_write: true };
const NO_NAME: MemberDriveAdminItem = { ...TEAM, id: "m3", name: "fixtures", team_unit_name: undefined };
const ORPHAN: MemberDriveAdminItem = { ...TEAM, id: "m4", name: "old-templates", team_unit_id: undefined, team_unit_name: undefined };
const PLAIN: MemberDriveAdminItem = { ...base, id: "m5", name: "Notes" };

const rowOf = (name: string) => screen.getByText(name, { selector: "td *, td" }).closest("tr")!;
const switchOf = (name: string) => screen.getByRole("switch", { name: T.TEAM_WRITE_SWITCH(name) });
const show = async (rows: MemberDriveAdminItem[]) => {
  listMemberDrives.mockResolvedValue({ drives: rows, total: rows.length });
  render(<MemberDrives />);
  await screen.findByTestId("member-drives");
};

beforeEach(() => {
  setMemberTeamDriveWrite.mockReset();
});
afterEach(() => {
  cleanup();
  listMemberDrives.mockReset();
});

describe("MemberDrives: a person's own team drive", () => {
  it("a team row shows its chips, unit name and switch", async () => {
    await show([TEAM, NO_NAME]);
    const row = within(rowOf(TEAM.name));
    expect(row.getByText(DRIVES.TEAM_CHIP)).toBeInTheDocument();
    expect(row.getByText(DRIVES.TEAM_ONE_HOST_CHIP)).toBeInTheDocument();
    expect(row.getByTestId("member-drive-unit")).toHaveTextContent("Platform");
    expect(within(rowOf(NO_NAME.name)).getByTestId("member-drive-unit")).toHaveTextContent(UNIT);
    expect(switchOf("design-assets")).toBeInTheDocument();
    expect(screen.getByText(DRIVES.TEAM_ONE_HOST)).toBeInTheDocument();
  });

  it("a team drive whose unit was deleted has no write control", async () => {
    await show([ORPHAN]);
    const row = within(rowOf(ORPHAN.name));
    expect(row.getByTestId("member-drive-unit")).toHaveTextContent(T.NONE);
    expect(row.getByTestId("member-drive-write")).toHaveTextContent(T.NONE);
    expect(screen.queryByRole("switch", { name: /old-templates/ })).toBeNull();
  });

  it("allow writing opens on Cancel", async () => {
    await show([TEAM]);
    await userEvent.click(switchOf(TEAM.name));
    const dlg = within(await screen.findByRole("alertdialog"));
    expect(dlg.getByText(T.TEAM_WRITE_ALLOW_TITLE(TEAM.name))).toBeInTheDocument();
    expect(dlg.getByText(T.TEAM_WRITE_ALLOW_BODY("Platform"))).toBeInTheDocument();
    await waitFor(() => expect(dlg.getByRole("button", { name: PEOPLE.CANCEL })).toHaveFocus());
  });

  it("make read-only opens on its button", async () => {
    await show([TEAM_ON]);
    await userEvent.click(switchOf(TEAM_ON.name));
    const dlg = within(await screen.findByRole("alertdialog"));
    expect(dlg.getByText(T.TEAM_WRITE_STOP_TITLE(TEAM_ON.name))).toBeInTheDocument();
    expect(dlg.getByText(T.TEAM_WRITE_STOP_BODY(TEAM_ON.owner))).toBeInTheDocument();
    await waitFor(() => expect(dlg.getByRole("button", { name: T.TEAM_WRITE_STOP })).toHaveFocus());
  });

  it("the switch flips only on 200", async () => {
    let answer: (d: MemberDriveAdminItem) => void = () => {};
    setMemberTeamDriveWrite.mockReturnValueOnce(new Promise<MemberDriveAdminItem>((done) => (answer = done)));
    await show([TEAM]);
    await userEvent.click(switchOf(TEAM.name));
    await userEvent.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: T.TEAM_WRITE_ALLOW }));
    expect(setMemberTeamDriveWrite).toHaveBeenCalledWith(TEAM.id, true);
    expect(switchOf(TEAM.name)).toHaveAttribute("aria-checked", "false");
    await act(async () => answer({ ...TEAM, members_can_write: true }));
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(switchOf(TEAM.name)).toHaveAttribute("aria-checked", "true");
    await waitFor(() => expect(switchOf(TEAM.name)).toHaveFocus());
  });

  it("Cancel returns focus to the switch", async () => {
    await show([TEAM]);
    await userEvent.click(switchOf(TEAM.name));
    await userEvent.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: PEOPLE.CANCEL }));
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    await waitFor(() => expect(switchOf(TEAM.name)).toHaveFocus());
    expect(switchOf(TEAM.name)).toHaveAttribute("aria-checked", "false");
    expect(setMemberTeamDriveWrite).not.toHaveBeenCalled();
  });

  it("a 409 shows the server's sentence and keeps the switch", async () => {
    setMemberTeamDriveWrite.mockRejectedValueOnce(new HttpError(409, "This drive is already being deleted.", "member_drive_busy"));
    await show([TEAM]);
    await userEvent.click(switchOf(TEAM.name));
    const dlg = within(await screen.findByRole("alertdialog"));
    await userEvent.click(dlg.getByRole("button", { name: T.TEAM_WRITE_ALLOW }));
    expect(await dlg.findByRole("alert")).toHaveTextContent("This drive is already being deleted.");
    expect(screen.getByRole("alertdialog")).toBeInTheDocument();
    expect(switchOf(TEAM.name)).toHaveAttribute("aria-checked", "false");
  });

  it("another failure shows the alert line", async () => {
    setMemberTeamDriveWrite.mockRejectedValueOnce(new HttpError(500, "The server could not save this change."));
    await show([TEAM_ON]);
    await userEvent.click(switchOf(TEAM_ON.name));
    const dlg = within(await screen.findByRole("alertdialog"));
    await userEvent.click(dlg.getByRole("button", { name: T.TEAM_WRITE_STOP }));
    expect(await dlg.findByRole("alert")).toHaveTextContent("The server could not save this change.");
    expect(switchOf(TEAM_ON.name)).toHaveAttribute("aria-checked", "true");
  });

  it("the write switch shows its state in words", async () => {
    await show([TEAM, TEAM_ON]);
    const off = within(rowOf(TEAM.name)).getByTestId("member-drive-write");
    const on = within(rowOf(TEAM_ON.name)).getByTestId("member-drive-write");
    expect(switchOf(TEAM.name)).toHaveAttribute("aria-checked", "false");
    expect(within(off).getByText(T.TEAM_WRITE_OFF)).toHaveAttribute("aria-hidden", "true");
    expect(switchOf(TEAM_ON.name)).toHaveAttribute("aria-checked", "true");
    expect(within(on).getByText(T.TEAM_WRITE_ON)).toHaveAttribute("aria-hidden", "true");
    expect(switchOf(TEAM.name)).toHaveAccessibleName(`${DRIVES.FIELD_TEAM_WRITE}: ${TEAM.name}`);
    expect(switchOf(TEAM_ON.name)).toHaveAccessibleName(`${DRIVES.FIELD_TEAM_WRITE}: ${TEAM_ON.name}`);
  });

  it("deleting a team drive says the unit loses it too", async () => {
    await show([TEAM, PLAIN, ORPHAN]);
    const title = async (name: string) => {
      await userEvent.click(screen.getByRole("button", { name: `${DRIVES.DELETE} ${name}` }));
      const dlg = await screen.findByRole("alertdialog");
      const text = within(dlg).getByRole("heading").textContent;
      await userEvent.click(within(dlg).getByRole("button", { name: PEOPLE.CANCEL }));
      await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
      return text;
    };
    expect(await title(TEAM.name)).toBe(NR_FILES.DELETE_CONFIRM_TEAM(TEAM.name));
    expect(await title(PLAIN.name)).toBe(NR_FILES.DELETE_CONFIRM(PLAIN.name));
    expect(await title(ORPHAN.name)).toBe(NR_FILES.DELETE_CONFIRM(ORPHAN.name));
  });
});
