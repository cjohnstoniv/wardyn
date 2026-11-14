/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Files panel (mock M7-Files): the strip and both tabs render the server's rules with the
// board's strings, an upload reports delivered or too large, and a blocked download lists nothing.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { HttpError, setExpectedPrincipal } from "../../../lib/api/core";
import { aheadByHours } from "../../../lib/test-clock";
import { makeApproval } from "../../../../test/factories";
import { DESKTOP_FILES as C } from "../../wardyn/copy";
import { DATA_OUT_RUN } from "../../wardyn/copy/data-out-run";

const listMock = vi.fn();
const uploadMock = vi.fn();
const reviewMock = vi.fn();
const downloadMock = vi.fn();
const listApprovalsMock = vi.fn();
vi.mock("../../../lib/api/desktop-files", () => ({
  desktopFiles: {
    list: (...a: unknown[]) => listMock(...a),
    upload: (...a: unknown[]) => uploadMock(...a),
    review: (...a: unknown[]) => reviewMock(...a),
    download: (...a: unknown[]) => downloadMock(...a),
  },
}));
vi.mock("../../../lib/api/approvals", () => ({
  approvals: { listApprovals: (...a: unknown[]) => listApprovalsMock(...a) },
}));
// The run's recorded policy: its data_out rule is what the panel's lines read.
const getPolicyMock = vi.fn();
vi.mock("../../../lib/api/runs", () => ({ runs: { getPolicy: (...a: unknown[]) => getPolicyMock(...a) } }));

import { DesktopFilesPanel, REVIEW_POLL_MS, transferSize } from "./desktop-files-panel";

const defaults = { paste_in: "allowed", copy_out: "blocked", upload: "allowed", download: "blocked" };

const pausedMsg = "Run is paused; resume it to read its sandbox";
const pausedRefusal = () => new HttpError(409, pausedMsg, "run_inspect_paused");

beforeEach(() => {
  listMock.mockReset();
  uploadMock.mockReset();
  reviewMock.mockReset();
  downloadMock.mockReset();
  listApprovalsMock.mockReset();
  listApprovalsMock.mockResolvedValue([]);
  getPolicyMock.mockReset();
  getPolicyMock.mockResolvedValue({ run_id: "r1", state: "never", source: { kind: "unknown" }, redacted: false, changes: [] });
});

afterEach(() => {
  vi.useRealTimers();
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

  it("a file sent after another person signed in still names the person who chose the files", async () => {
    // The server then refuses it rather than put it in the desktop as the next person.
    listMock.mockResolvedValue({ folder: "~/Transfers", rules: defaults, files: [] });
    let first!: (v: unknown) => void;
    uploadMock.mockImplementationOnce(() => new Promise((done) => (first = done))).mockResolvedValue({ name: "b.txt", bytes: 1, sha256: "0".repeat(64) });
    setExpectedPrincipal("a@example.com");
    render(<DesktopFilesPanel runId="r1" />);
    const panel = await screen.findByRole("region", { name: C.title });
    await userEvent.upload(within(panel).getByLabelText(C.choose), [new File(["a"], "a.txt"), new File(["b"], "b.txt")]);
    await waitFor(() => expect(uploadMock).toHaveBeenCalledTimes(1));

    setExpectedPrincipal("b@example.com");
    first({ name: "a.txt", bytes: 1, sha256: "0".repeat(64) });
    await waitFor(() => expect(uploadMock).toHaveBeenCalledTimes(2));
    expect(uploadMock.mock.calls.map((c) => c[2])).toEqual(["a@example.com", "a@example.com"]);
    setExpectedPrincipal("");
  });

  it("on a VM desktop says copy and paste are not available instead of the Allowed strip", async () => {
    listMock.mockResolvedValue({ folder: "~/Transfers", rules: defaults, files: [] });
    render(<DesktopFilesPanel runId="r1" vm />);
    const panel = await screen.findByRole("region", { name: C.title });
    expect(within(panel).getByText("Copy and paste: not available on VM desktops yet")).toBeTruthy();
    expect(within(panel).queryByText(C.pasteIn)).toBeNull();
    expect(within(panel).queryByText(C.copyOut)).toBeNull();
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
    expect(ok.textContent).toMatch(/sha256 c3a1…0e9d · [^·]*\d{1,2}:\d\d[^·]*/);
    await waitFor(() => expect(big.textContent).toContain(C.tooLarge));
    expect(big.textContent).toContain(C.tooLargeSub);
  });

  it("a failed upload says Upload failed with the server's sentence, never Not sent", async () => {
    listMock.mockResolvedValue({ folder: "~/Transfers", rules: defaults, files: [] });
    const sentence = "the upload to ~/Transfers failed, and some bytes may remain there.";
    uploadMock.mockRejectedValueOnce(new HttpError(502, sentence, "desktop_transfer_failed"));
    render(<DesktopFilesPanel runId="r1" />);
    const panel = await screen.findByRole("region", { name: C.title });
    await userEvent.upload(within(panel).getByLabelText(C.choose), new File(["a,b,c"], "notes.csv"));
    const row = await within(panel).findByTestId("desktop-upload");
    await waitFor(() => expect(row.textContent).toContain(C.uploadFailed));
    expect(row.textContent).toContain(sentence);
    expect(row.textContent).not.toContain("Not sent");
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

  it("opened on a paused run, says it is paused and comes back by itself once the run wakes", async () => {
    vi.useFakeTimers();
    listMock.mockRejectedValueOnce(pausedRefusal());
    render(<DesktopFilesPanel runId="r1" />);
    await act(() => vi.advanceTimersByTimeAsync(0));
    expect(screen.getByTestId("desktop-files-paused").textContent).toBe(pausedMsg);
    expect(screen.queryByText(pausedMsg)?.className).not.toContain("font-mono");

    listMock.mockRejectedValueOnce(pausedRefusal());
    await act(() => vi.advanceTimersByTimeAsync(4000));
    expect(listMock).toHaveBeenCalledTimes(2);
    expect(screen.getByTestId("desktop-files-paused").textContent).toBe(pausedMsg);

    listMock.mockResolvedValue({ folder: "~/Transfers", rules: { ...defaults, download: "allowed" }, files: [] });
    await act(() => vi.advanceTimersByTimeAsync(4000));
    expect(screen.queryByTestId("desktop-files-paused")).toBeNull();
    expect(screen.getByText(C.drop)).toBeTruthy();

    await act(() => vi.advanceTimersByTimeAsync(20000));
    expect(listMock).toHaveBeenCalledTimes(3);
  });

  it("a run paused after the panel loaded keeps its rules and its upload, and an allowed upload still goes", async () => {
    listMock.mockResolvedValueOnce({ folder: "~/Transfers", rules: { ...defaults, download: "allowed" }, files: [{ name: "report.csv", bytes: 12 }] });
    uploadMock.mockResolvedValue({ name: "notes.csv", bytes: 5, sha256: "c3a1" + "0".repeat(56) + "0e9d" });
    render(<DesktopFilesPanel runId="r1" />);
    const panel = await screen.findByRole("region", { name: C.title });
    listMock.mockRejectedValue(pausedRefusal());
    await userEvent.click(within(panel).getByRole("tab", { name: C.fromTab }));
    expect((await within(panel).findByTestId("desktop-files-paused")).textContent).toBe(pausedMsg);
    expect(within(panel).getByTestId("desktop-download").textContent).toContain("report.csv");
    expect(within(panel).queryByText(C.emptyFolder)).toBeNull();

    await userEvent.click(within(panel).getByRole("tab", { name: C.toTab }));
    await userEvent.upload(within(panel).getByLabelText(C.choose), new File(["a,b,c"], "notes.csv"));
    await waitFor(() => expect(within(panel).getByTestId("desktop-upload").textContent).toContain(C.delivered));
  });

  it("any other refused load is still the load error, and is not polled", async () => {
    vi.useFakeTimers();
    listMock.mockRejectedValue(new HttpError(409, "run is not RUNNING; no files can move (state=STOPPED)", "attach_not_running"));
    render(<DesktopFilesPanel runId="r1" />);
    await act(() => vi.advanceTimersByTimeAsync(0));
    expect(screen.getByText(/no files can move/).className).toContain("font-mono");
    expect(screen.queryByTestId("desktop-files-paused")).toBeNull();
    await act(() => vi.advanceTimersByTimeAsync(20000));
    expect(listMock).toHaveBeenCalledTimes(1);
  });

  it("writes sizes the way the board does", () => {
    expect([48 * 1024, 2.4 * 1024 ** 2, 2.1 * 1024 ** 3].map(transferSize)).toEqual(["48 KB", "2.4 MB", "2.1 GB"]);
  });
});

// Under desktop.download: review, the From tab lists the files, one row each.
const reviewing = { ...defaults, download: "review" };
const SHA = "9f2c" + "0".repeat(56) + "41ab";
const fileRow = (o: Parameters<typeof makeApproval>[0]) =>
  makeApproval({ id: "ap_1", kind: "file_download", requested_scope: { name: "a.csv", bytes: 84 * 1024, sha256: SHA, requested_by: "dana@acme.example" }, ...o });

async function openFrom(files = [{ name: "a.csv", bytes: 84 * 1024 }]) {
  listMock.mockResolvedValue({ folder: "~/Transfers", rules: reviewing, files });
  render(<DesktopFilesPanel runId="r1" />);
  const panel = await screen.findByRole("region", { name: C.title });
  await userEvent.click(within(panel).getByRole("tab", { name: C.fromTab }));
  return panel;
}

describe("DesktopFilesPanel under review", () => {
  it("lists the files with Request download and the review footer, not the allowed footer", async () => {
    const panel = await openFrom([{ name: "a.csv", bytes: 84 * 1024 }, { name: "b.csv", bytes: 3 }]);
    const rows = await within(panel).findAllByTestId("desktop-download");
    expect(rows).toHaveLength(2);
    expect(within(rows[0]).getByRole("button", { name: C.requestDownload })).toBeTruthy();
    expect(within(panel).getByText(C.reviewFooter)).toBeTruthy();
    expect(within(panel).queryByText(C.downloadsAllowed)).toBeNull();
    expect(within(panel).queryByText(C.downloadBlocked)).toBeNull();
    expect(listApprovalsMock).toHaveBeenCalledWith("", "r1");
  });

  it("shows each file's newest request: the older denied one does not hide the newer waiting one", async () => {
    listApprovalsMock.mockResolvedValue([
      fileRow({ id: "old", state: "DENIED", requested_at: aheadByHours(-3) }),
      fileRow({ id: "new", state: "PENDING", requested_at: aheadByHours(-1) }),
      makeApproval({ id: "other", kind: "egress_domain", state: "APPROVED" }),
    ]);
    const panel = await openFrom();
    expect(await within(panel).findByText(C.stateWaiting)).toBeTruthy();
    expect(within(panel).queryByText(C.stateDenied)).toBeNull();
  });

  it("blocked keeps the Blocked note and never reads the requests", async () => {
    listMock.mockResolvedValue({ folder: "~/Transfers", rules: defaults, files: [{ name: "a.csv", bytes: 1 }] });
    render(<DesktopFilesPanel runId="r1" />);
    const panel = await screen.findByRole("region", { name: C.title });
    await userEvent.click(within(panel).getByRole("tab", { name: C.fromTab }));
    expect(await within(panel).findByText(C.downloadBlocked)).toBeTruthy();
    expect(within(panel).queryByText(C.reviewFooter)).toBeNull();
    expect(listApprovalsMock).not.toHaveBeenCalled();
  });

  it("allowed is unchanged: Download, the allowed footer, no request read", async () => {
    listMock.mockResolvedValue({ folder: "~/Transfers", rules: { ...defaults, download: "allowed" }, files: [{ name: "a.csv", bytes: 1 }] });
    render(<DesktopFilesPanel runId="r1" />);
    const panel = await screen.findByRole("region", { name: C.title });
    await userEvent.click(within(panel).getByRole("tab", { name: C.fromTab }));
    expect(await within(panel).findByRole("button", { name: `${C.download} a.csv` })).toBeTruthy();
    expect(within(panel).getByText(C.downloadsAllowed)).toBeTruthy();
    expect(within(panel).queryByText(C.reviewFooter)).toBeNull();
    expect(listApprovalsMock).not.toHaveBeenCalled();
  });

  it("an unreadable request list says so, keeps Request download, and Try again reads it again", async () => {
    listApprovalsMock.mockRejectedValue(new Error("down"));
    const panel = await openFrom();
    expect(await within(panel).findByText(C.requestsUnreadable)).toBeTruthy();
    expect(within(panel).getByRole("button", { name: C.requestDownload })).toBeTruthy();
    const reads = listApprovalsMock.mock.calls.length;
    listApprovalsMock.mockResolvedValue([fileRow({})]);
    await userEvent.click(within(panel).getByRole("button", { name: C.retry }));
    expect(await within(panel).findByText(C.stateWaiting)).toBeTruthy();
    expect(listApprovalsMock.mock.calls.length).toBe(reads + 1);
    expect(within(panel).queryByText(C.requestsUnreadable)).toBeNull();
  });

  it("Request download asks, then shows the returned pending row at once", async () => {
    reviewMock.mockResolvedValue(fileRow({}));
    const panel = await openFrom();
    await userEvent.click(await within(panel).findByRole("button", { name: C.requestDownload }));
    expect(reviewMock).toHaveBeenCalledWith("r1", "a.csv");
    expect(await within(panel).findByText(C.stateWaiting)).toBeTruthy();
    expect(within(panel).queryByRole("button", { name: C.requestDownload })).toBeNull();
  });

  it.each([
    [413, "desktop_transfer_too_large", "the file is over the 256 MiB limit for a reviewed download. Nothing was asked."],
    [503, "desktop_review_unavailable", "the download's review could not be raised. Nothing was asked for or sent; try again."],
    [429, "internal_approval_cap_reached", "this run has raised too many approvals; no more will be accepted"],
  ])("a %i %s refusal of the ask prints the server's sentence under the row", async (status, reason, sentence) => {
    reviewMock.mockRejectedValue(new HttpError(status, sentence, reason));
    const panel = await openFrom();
    await userEvent.click(await within(panel).findByRole("button", { name: C.requestDownload }));
    expect(await within(panel).findByText(`${C.notAsked}: ${sentence}`)).toBeTruthy();
    expect(within(panel).getByRole("button", { name: C.requestDownload })).toBeTruthy();
  });

  it("a 409 already-waiting refusal re-reads the requests and shows Waiting", async () => {
    const sentence = "a review of this file is already waiting; it is released once a person approves it";
    reviewMock.mockRejectedValue(new HttpError(409, sentence, "desktop_review_pending"));
    // The server already holds the waiting request: every read after the refused ask returns it.
    listApprovalsMock.mockImplementation(async () => (reviewMock.mock.calls.length > 0 ? [fileRow({})] : []));
    const panel = await openFrom();
    await userEvent.click(await within(panel).findByRole("button", { name: C.requestDownload }));
    await within(panel).findByText(`${C.notAsked}: ${sentence}`);
    expect(await within(panel).findByText(C.stateWaiting)).toBeTruthy();
  });

  it.each([
    [409, "file_changed_since_review", "the file changed after it was reviewed: its bytes are not the ones approved. Nothing was sent; ask for another review."],
    [409, "desktop_review_spent", "that approval already released its file; ask for another review. Nothing was sent."],
    [403, "desktop_review_not_approved", "that approval is not an approved review of this file on this run. Nothing was sent."],
  ])("a %i %s refusal of Download approved prints Download failed with the sentence", async (status, reason, sentence) => {
    listApprovalsMock.mockResolvedValue([fileRow({ state: "APPROVED", decided_by: "lee@acme.example", decided_at: aheadByHours(-1) })]);
    downloadMock.mockRejectedValue(new HttpError(status, sentence, reason));
    const panel = await openFrom();
    await userEvent.click(await within(panel).findByRole("button", { name: C.downloadApproved }));
    expect(downloadMock).toHaveBeenCalledWith("r1", "a.csv", "ap_1");
    expect(await within(panel).findByText(`${C.downloadFailed}: ${sentence}`)).toBeTruthy();
    expect(within(panel).getByRole("button", { name: C.downloadApproved })).toBeTruthy();
  });

  it("a request approved while the From tab is open shows Approved on the next read", async () => {
    vi.useFakeTimers();
    listMock.mockResolvedValue({ folder: "~/Transfers", rules: reviewing, files: [{ name: "a.csv", bytes: 84 * 1024 }] });
    listApprovalsMock.mockResolvedValue([fileRow({})]);
    render(<DesktopFilesPanel runId="r1" />);
    await act(() => vi.advanceTimersByTimeAsync(0));
    fireEvent.mouseDown(screen.getByRole("tab", { name: C.fromTab }), { button: 0 });
    await act(() => vi.advanceTimersByTimeAsync(0));
    expect(screen.getByText(C.stateWaiting)).toBeTruthy();
    expect(screen.queryByRole("button", { name: C.downloadApproved })).toBeNull();

    listApprovalsMock.mockResolvedValue([fileRow({ state: "APPROVED", decided_by: "lee@acme.example", decided_at: aheadByHours(0) })]);
    await act(() => vi.advanceTimersByTimeAsync(REVIEW_POLL_MS));
    expect(screen.getByText(C.stateApproved)).toBeTruthy();
    expect(screen.getByRole("button", { name: C.downloadApproved })).toBeTruthy();
  });

  it("does not read the requests on a timer while the To tab is showing", async () => {
    vi.useFakeTimers();
    listMock.mockResolvedValue({ folder: "~/Transfers", rules: reviewing, files: [{ name: "a.csv", bytes: 1 }] });
    render(<DesktopFilesPanel runId="r1" />);
    await act(() => vi.advanceTimersByTimeAsync(0));
    const reads = listApprovalsMock.mock.calls.length;
    await act(() => vi.advanceTimersByTimeAsync(REVIEW_POLL_MS * 3));
    expect(listApprovalsMock.mock.calls.length).toBe(reads);
  });
});

// The run's data_out rule on the panel: a line under a refused download, the no-desktop layout, and the
// refusals the server answers with.
const recorded = (data_out?: string) => ({
  run_id: "r1",
  state: "recorded",
  source: { kind: "default" },
  spec: data_out ? { data_out } : {},
  redacted: false,
  changes: [],
});

describe("DesktopFilesPanel on a run that keeps its content in", () => {
  it("under a blocked download says the run's data_out rule keeps its content in", async () => {
    getPolicyMock.mockResolvedValue(recorded("blocked"));
    listMock.mockResolvedValue({ folder: "~/Transfers", rules: defaults, files: [] });
    render(<DesktopFilesPanel runId="r1" />);
    const panel = await screen.findByRole("region", { name: C.title });
    await userEvent.click(within(panel).getByRole("tab", { name: C.fromTab }));
    expect(await within(panel).findByText(DATA_OUT_RUN.FILES_BLOCKED)).toBeTruthy();
    expect(within(panel).getByText(C.downloadBlocked)).toBeTruthy();
  });

  it("draws no such line while the rule is unread, failed, or allowed", async () => {
    for (const read of [() => new Promise(() => {}), () => Promise.reject(new Error("down")), () => Promise.resolve(recorded())]) {
      getPolicyMock.mockReset();
      getPolicyMock.mockImplementation(read);
      listMock.mockResolvedValue({ folder: "~/Transfers", rules: defaults, files: [] });
      const { unmount } = render(<DesktopFilesPanel runId="r1" />);
      const panel = await screen.findByRole("region", { name: C.title });
      await userEvent.click(within(panel).getByRole("tab", { name: C.fromTab }));
      expect(await within(panel).findByText(C.downloadBlocked)).toBeTruthy();
      await waitFor(() => expect(getPolicyMock).toHaveBeenCalled());
      expect(within(panel).queryByText(DATA_OUT_RUN.FILES_BLOCKED)).toBeNull();
      unmount();
    }
  });

  it("with no desktop hides the strip and the To tab, and says only the reviewed way out is left", async () => {
    listMock.mockResolvedValue({ folder: "~/Transfers", rules: reviewing, files: [{ name: "results.parquet", bytes: 6.3 * 1024 ** 2 }] });
    render(<DesktopFilesPanel runId="r1" noDesktop />);
    const panel = await screen.findByRole("region", { name: C.title });
    expect(within(panel).getByText(DATA_OUT_RUN.FILES_NO_DESKTOP)).toBeTruthy();
    expect(panel.textContent).toContain(DATA_OUT_RUN.FOLDER_IN_RUN);
    expect(panel.textContent).not.toContain(C.folderWhere);
    expect(within(panel).queryByText(C.pasteIn)).toBeNull();
    expect(within(panel).queryByText(C.copyOut)).toBeNull();
    expect(within(panel).queryByRole("tab")).toBeNull();
    expect(within(panel).queryByText(C.drop)).toBeNull();
    const row = await within(panel).findByTestId("desktop-download");
    expect(within(row).getByRole("button", { name: C.requestDownload })).toBeTruthy();
  });

  it("with a desktop keeps the strip, both tabs and the desktop folder words", async () => {
    listMock.mockResolvedValue({ folder: "~/Transfers", rules: reviewing, files: [] });
    render(<DesktopFilesPanel runId="r1" />);
    const panel = await screen.findByRole("region", { name: C.title });
    expect(within(panel).getAllByRole("tab")).toHaveLength(2);
    expect(panel.textContent).toContain(C.folderWhere);
    expect(within(panel).queryByText(DATA_OUT_RUN.FILES_NO_DESKTOP)).toBeNull();
  });

  it("an unreadable rule on load shows its sentence with Try again, which reads again", async () => {
    const unreadable = "This run's data_out rule could not be read. Try again.";
    listMock.mockRejectedValueOnce(new HttpError(503, unreadable, "data_out_unreadable"));
    listMock.mockResolvedValue({ folder: "~/Transfers", rules: defaults, files: [] });
    render(<DesktopFilesPanel runId="r1" />);
    expect(await screen.findByText(unreadable)).toBeTruthy();
    await userEvent.click(screen.getByRole("button", { name: DATA_OUT_RUN.RETRY }));
    const panel = await screen.findByRole("region", { name: C.title });
    expect(within(panel).queryByText(unreadable)).toBeNull();
    expect(listMock).toHaveBeenCalledTimes(2);
  });

  it("a review that cannot move files prints the server's sentence and no Try again", async () => {
    const unavailable = "This run's machine cannot move files through Wardyn; nothing can leave it while data_out is review.";
    listMock.mockRejectedValue(new HttpError(409, unavailable, "data_out_review_unavailable"));
    render(<DesktopFilesPanel runId="r1" noDesktop />);
    expect(await screen.findByText(unavailable)).toBeTruthy();
    expect(screen.queryByRole("button", { name: DATA_OUT_RUN.RETRY })).toBeNull();
  });
  it("a dropped connection says so in a sentence and Try again reads the folder again", async () => {
    listMock.mockRejectedValueOnce(new TypeError("Failed to fetch"));
    listMock.mockResolvedValue({ folder: "~/Transfers", rules: defaults, files: [] });
    render(<DesktopFilesPanel runId="r1" />);
    expect(await screen.findByText(C.loadFailed)).toBeTruthy();
    expect(screen.queryByText("Failed to fetch")).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: C.retry }));
    const panel = await screen.findByRole("region", { name: C.title });
    expect(within(panel).queryByText(C.loadFailed)).toBeNull();
    expect(listMock).toHaveBeenCalledTimes(2);
  });

  it("a server error gets the same sentence and Try again, not the server's raw text", async () => {
    listMock.mockRejectedValue(new HttpError(500, "Internal Server Error", "internal"));
    render(<DesktopFilesPanel runId="r1" />);
    expect(await screen.findByText(C.loadFailed)).toBeTruthy();
    expect(screen.queryByText("Internal Server Error")).toBeNull();
    expect(screen.getByRole("button", { name: C.retry })).toBeTruthy();
  });

  it("a Download in flight is disabled, so a second click starts no second transfer", async () => {
    listMock.mockResolvedValue({ folder: "~/Transfers", rules: { ...defaults, download: "allowed" }, files: [{ name: "a.csv", bytes: 12 }] });
    let finish: (v: { blob: Blob; sha256: string }) => void = () => {};
    downloadMock.mockReturnValue(new Promise((r) => (finish = r)));
    URL.createObjectURL = vi.fn(() => "blob:x");
    URL.revokeObjectURL = vi.fn();
    render(<DesktopFilesPanel runId="r1" />);
    const panel = await screen.findByRole("region", { name: C.title });
    await userEvent.click(within(panel).getByRole("tab", { name: C.fromTab }));
    const button = await within(panel).findByRole("button", { name: `${C.download} a.csv` });
    await userEvent.click(button);
    expect(button).toBeDisabled();
    await userEvent.click(button);
    expect(downloadMock).toHaveBeenCalledTimes(1);
    finish({ blob: new Blob(["x"]), sha256: "abcd".repeat(16) });
    await waitFor(() => expect(button).toBeEnabled());
  });

  it("a Download revokes the object URL only after the click's task, so the browser has the file first", async () => {
    listMock.mockResolvedValue({ folder: "~/Transfers", rules: { ...defaults, download: "allowed" }, files: [{ name: "a.csv", bytes: 12 }] });
    downloadMock.mockResolvedValue({ blob: new Blob(["x"]), sha256: "abcd".repeat(16) });
    URL.createObjectURL = vi.fn(() => "blob:x");
    URL.revokeObjectURL = vi.fn();
    const atClick: number[] = [];
    const click = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(() => {
      setTimeout(() => atClick.push(vi.mocked(URL.revokeObjectURL).mock.calls.length), 0); // a task queued at the click
    });
    render(<DesktopFilesPanel runId="r1" />);
    const panel = await screen.findByRole("region", { name: C.title });
    await userEvent.click(within(panel).getByRole("tab", { name: C.fromTab }));
    await userEvent.click(await within(panel).findByRole("button", { name: `${C.download} a.csv` }));
    await waitFor(() => expect(URL.revokeObjectURL).toHaveBeenCalledWith("blob:x"));
    expect(atClick).toEqual([0]);
    click.mockRestore();
  });
});
