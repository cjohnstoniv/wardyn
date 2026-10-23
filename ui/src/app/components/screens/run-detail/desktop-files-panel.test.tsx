/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Files panel (mock M7-Files): the strip and both tabs render the server's rules with the
// board's strings, an upload reports delivered or too large, and a blocked download lists nothing.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { HttpError } from "../../../lib/api/core";
import { DESKTOP_FILES as C } from "../../wardyn/copy";

const listMock = vi.fn();
const uploadMock = vi.fn();
vi.mock("../../../lib/api/desktop-files", () => ({
  desktopFiles: { list: (...a: unknown[]) => listMock(...a), upload: (...a: unknown[]) => uploadMock(...a), download: vi.fn() },
}));

import { DesktopFilesPanel, transferSize } from "./desktop-files-panel";

const defaults = { paste_in: "allowed", copy_out: "blocked", upload: "allowed", download: "blocked" };

beforeEach(() => {
  listMock.mockReset();
  uploadMock.mockReset();
});

describe("DesktopFilesPanel", () => {
  it("shows the clipboard strip and a blocked download that lists nothing", async () => {
    listMock.mockResolvedValue({ folder: "~/Transfers", rules: defaults, files: [{ name: "secret.txt", bytes: 3 }] });
    render(<DesktopFilesPanel runId="r1" />);
    const panel = await screen.findByRole("region", { name: C.title });
    expect(within(panel).getByText(C.pasteIn).nextSibling?.textContent).toBe(C.allowed);
    expect(within(panel).getByText(C.copyOut).nextSibling?.textContent).toBe(C.blocked);
    expect(within(panel).getByText("~/Transfers")).toBeTruthy();
    await userEvent.click(within(panel).getByRole("tab", { name: C.fromTab }));
    expect(await within(panel).findByText(C.downloadBlocked)).toBeTruthy();
    expect(within(panel).getByText(C.refused)).toBeTruthy();
    expect(within(panel).getByText(C.blockedEmpty)).toBeTruthy();
    expect(within(panel).getByText(C.refusedLogged)).toBeTruthy();
    expect(within(panel).queryByText("secret.txt")).toBeNull();
  });

  it("reports a delivered upload by hash and a too-large one as not sent", async () => {
    listMock.mockResolvedValue({ folder: "~/Transfers", rules: defaults, files: [] });
    uploadMock
      .mockResolvedValueOnce({ name: "notes.csv", bytes: 5, sha256: "c3a1" + "0".repeat(56) + "0e9d" })
      .mockRejectedValueOnce(new HttpError(413, "over the limit"));
    render(<DesktopFilesPanel runId="r1" />);
    const panel = await screen.findByRole("region", { name: C.title });
    expect(within(panel).getByText(C.drop)).toBeTruthy();
    expect(within(panel).getByText(C.dropSub)).toBeTruthy();
    expect(within(panel).getByText(C.everyFile)).toBeTruthy();
    const input = within(panel).getByLabelText(C.choose) as HTMLInputElement;
    await userEvent.upload(input, [new File(["a,b,c"], "notes.csv"), new File(["x"], "disk-image.iso")]);
    await waitFor(() => expect(within(panel).getAllByTestId("desktop-upload")).toHaveLength(2));
    const [ok, big] = within(panel).getAllByTestId("desktop-upload");
    await waitFor(() => expect(ok.textContent).toContain(C.delivered));
    expect(ok.textContent).toMatch(/sha256 c3a1…0e9d · \d\d:\d\d/);
    await waitFor(() => expect(big.textContent).toContain(C.tooLarge));
    expect(big.textContent).toContain(C.tooLargeSub);
  });

  it("lists the folder with a Download button when download is allowed", async () => {
    listMock.mockResolvedValue({ folder: "~/Transfers", rules: { ...defaults, download: "allowed" }, files: [{ name: "refund-report.csv", bytes: 12 * 1024 }] });
    render(<DesktopFilesPanel runId="r1" />);
    const panel = await screen.findByRole("region", { name: C.title });
    await userEvent.click(within(panel).getByRole("tab", { name: C.fromTab }));
    const row = await within(panel).findByTestId("desktop-download");
    expect(row.textContent).toContain("refund-report.csv");
    expect(row.textContent).toContain("12 KB");
    expect(within(row).getByRole("button", { name: `${C.download} refund-report.csv` })).toBeTruthy();
    expect(within(panel).getByText(C.downloadsAllowed)).toBeTruthy();
  });

  it("writes sizes the way the board does", () => {
    expect([48 * 1024, 2.4 * 1024 ** 2, 2.1 * 1024 ** 3].map(transferSize)).toEqual(["48 KB", "2.4 MB", "2.1 GB"]);
  });
});
