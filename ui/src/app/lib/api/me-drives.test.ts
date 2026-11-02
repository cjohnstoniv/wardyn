/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// deleteMyDrive: null on a 204, the server's result on the 202 and 200 of a disk drive, an HttpError on a refusal.
import { afterEach, describe, expect, it, vi } from "vitest";
import { HttpError } from "./core";
import { DRIVE_REFUSAL } from "../drive-refusals";
import { deleteMyDrive } from "./me-drives";

const wfetchMock = vi.fn();
vi.mock("./core", async (orig) => ({ ...(await orig<typeof import("./core")>()), wfetch: (...a: unknown[]) => wfetchMock(...a) }));

const json = (body: unknown, status: number) => new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });
afterEach(() => wfetchMock.mockReset());

describe("deleteMyDrive", () => {
  it("answers null for a delete that finished (204)", async () => {
    wfetchMock.mockResolvedValue(new Response(null, { status: 204 }));
    expect(await deleteMyDrive("d 1")).toBeNull();
    expect(wfetchMock).toHaveBeenCalledWith("/me/drives/d%201", { method: "DELETE" });
  });
  it("answers the result for a Mac that is offline (202) and one no longer registered (200)", async () => {
    const deferred = { reason: "member_drive_delete_deferred", message: DRIVE_REFUSAL.MEMBER_DELETE_DEFERRED("kofi-mbp") };
    wfetchMock.mockResolvedValue(json(deferred, 202));
    expect(await deleteMyDrive("d1")).toEqual(deferred);
    const recordOnly = { reason: "member_drive_record_only", message: DRIVE_REFUSAL.MEMBER_RECORD_ONLY("old-mac-mini") };
    wfetchMock.mockResolvedValue(json(recordOnly, 200));
    expect(await deleteMyDrive("d1")).toEqual(recordOnly);
  });
  it("throws the server's sentence for a drive in a run (409)", async () => {
    wfetchMock.mockResolvedValue(json({ error: DRIVE_REFUSAL.MEMBER_IN_RUN_ON_MAC("Notes", "mac-studio-07"), reason: "member_drive_in_use" }, 409));
    await expect(deleteMyDrive("d1")).rejects.toMatchObject({ status: 409, message: DRIVE_REFUSAL.MEMBER_IN_RUN_ON_MAC("Notes", "mac-studio-07") });
    await expect(deleteMyDrive("d1")).rejects.toBeInstanceOf(HttpError);
  });
});
