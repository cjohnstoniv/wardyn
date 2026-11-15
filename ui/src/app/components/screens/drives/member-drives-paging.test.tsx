/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// People's drives on the admin Drives screen, paged by the server's total: a page that answers after a delete read
// the list again from the top must not show a drive twice or make the next Show more skip one.
import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { MemberDriveAdminItem } from "../../../lib/api/drives";
import { aheadByHours } from "../../../lib/test-clock";
import { DRIVES } from "../../../lib/user-drives-copy";
import { MEMBER_DRIVES as T } from "../../wardyn/copy/member-drives";
import { MemberDrives } from "./member-drives";

const listMemberDrives = vi.fn();
vi.mock("../../../lib/api/drives", async (orig) => ({
  ...(await orig<typeof import("../../../lib/api/drives")>()),
  drives: {
    listMemberDrives: (...a: unknown[]) => listMemberDrives(...a),
    deleteMemberDrive: () => Promise.resolve({ outcome: "deleted" }),
  },
}));
vi.mock("../../../lib/api/runners", () => ({ runners: { list: () => Promise.resolve([]) } }));

const drive = (id: string, name: string): MemberDriveAdminItem => ({
  id, name, owner: "lena@example.com", backend: "docker_volume", object: "vol", created_at: aheadByHours(-1), deleting: false,
});
const [A, B, C, D, E] = [drive("m1", "Notes"), drive("m2", "Build cache"), drive("m3", "Scratch"), drive("m4", "Logs"), drive("m5", "Fixtures")];
type Page = { drives: MemberDriveAdminItem[]; total: number };
const later = () => {
  let answer: (p: Page) => void = () => {};
  const promise = new Promise<Page>((done) => (answer = done));
  return { promise, answer: (drives: MemberDriveAdminItem[], total: number) => act(async () => answer({ drives, total })) };
};
const del = async (name: string) => {
  await userEvent.click(screen.getByRole("button", { name: `${DRIVES.DELETE} ${name}` }));
  await userEvent.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: DRIVES.DELETE }));
};

afterEach(() => {
  cleanup();
  listMemberDrives.mockReset();
});

describe("MemberDrives: a page still out when the list is read again", () => {
  it("drops a later page asked for before a delete read the list again", async () => {
    const page2 = later();
    listMemberDrives
      .mockResolvedValueOnce({ drives: [A, B], total: 3 })
      .mockReturnValueOnce(page2.promise)
      .mockResolvedValueOnce({ drives: [B, C], total: 2 });
    render(<MemberDrives />);
    const s = within(await screen.findByTestId("member-drives"));
    await userEvent.click(s.getByRole("button", { name: T.SHOW_MORE }));
    await del("Notes");
    await waitFor(() => expect(s.queryByText("Notes")).toBeNull());
    await page2.answer([C], 3);
    expect(s.getAllByText("Scratch")).toHaveLength(1);
    // Its count is dropped too: two of two leaves nothing more to show.
    expect(s.queryByRole("button", { name: T.SHOW_MORE })).toBeNull();
  });

  it("a later page that fails after a delete read the list again leaves the new list on screen", async () => {
    let fail: (e: Error) => void = () => {};
    listMemberDrives
      .mockResolvedValueOnce({ drives: [A, B], total: 3 })
      .mockReturnValueOnce(new Promise<Page>((_, no) => (fail = no)))
      .mockResolvedValueOnce({ drives: [B, C], total: 2 });
    render(<MemberDrives />);
    const s = within(await screen.findByTestId("member-drives"));
    await userEvent.click(s.getByRole("button", { name: T.SHOW_MORE }));
    await del("Notes");
    await waitFor(() => expect(s.queryByText("Notes")).toBeNull());
    await act(async () => fail(new Error("offline")));
    expect(s.getByText("Scratch")).toBeInTheDocument();
    expect(s.queryByText(DRIVES.FETCH_FAILED_TITLE)).toBeNull();
  });

  it("drops a later page asked for after rows that the new first page has replaced", async () => {
    const first = later();
    const page3 = later();
    listMemberDrives
      .mockResolvedValueOnce({ drives: [A, B], total: 6 })
      .mockResolvedValueOnce({ drives: [C, D], total: 6 })
      .mockReturnValueOnce(first.promise)
      .mockReturnValueOnce(page3.promise);
    render(<MemberDrives />);
    const s = within(await screen.findByTestId("member-drives"));
    await userEvent.click(s.getByRole("button", { name: T.SHOW_MORE }));
    await s.findByText("Logs");
    await del("Notes");
    await waitFor(() => expect(listMemberDrives).toHaveBeenCalledTimes(3));
    await userEvent.click(s.getByRole("button", { name: T.SHOW_MORE }));
    expect(listMemberDrives).toHaveBeenLastCalledWith(4);
    await first.answer([B, C], 5);
    await page3.answer([E], 5);
    // Offset 4 was asked of four rows; the list now holds two, so drives at offsets 2 and 3 would be skipped.
    expect(s.queryByText("Fixtures")).toBeNull();
    expect(s.getByText(T.SHOWING(2, 5))).toBeInTheDocument();
  });
});
