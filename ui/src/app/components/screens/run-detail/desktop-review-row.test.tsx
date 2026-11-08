/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// One Files row under desktop.download: review, one case per state of the board's first frame.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { HttpError } from "../../../lib/api/core";
import { aheadByHours } from "../../../lib/test-clock";
import { makeApproval } from "../../../../test/factories";
import { DESKTOP_FILES as C } from "../../wardyn/copy";
import { DECIDERS_INBOX } from "../../wardyn/copy/deciders-inbox";

const reviewMock = vi.fn();
const downloadMock = vi.fn();
vi.mock("../../../lib/api/desktop-files", () => ({
  desktopFiles: { review: (...a: unknown[]) => reviewMock(...a), download: (...a: unknown[]) => downloadMock(...a) },
}));

import { DesktopReviewRow } from "./desktop-review-row";

const SHA = "9f2c" + "0".repeat(56) + "41ab";
const file = { name: "build-logs.tar.gz", bytes: 19_293_184 };
const scope = { name: file.name, bytes: file.bytes, sha256: SHA, requested_by: "dana@acme.example" };
const row = (o: Parameters<typeof makeApproval>[0]) =>
  makeApproval({ id: "ap_1", kind: "file_download", requested_scope: scope, ...o });
const HHMM = /\d\d:\d\d/;

function renderRow(latest?: ReturnType<typeof makeApproval>) {
  const onAsked = vi.fn();
  const onRefused = vi.fn();
  render(
    <ul>
      <DesktopReviewRow runId="r1" file={file} latest={latest} onAsked={onAsked} onRefused={onRefused} />
    </ul>,
  );
  return { onAsked, onRefused };
}

beforeEach(() => {
  reviewMock.mockReset();
  downloadMock.mockReset();
  URL.createObjectURL = vi.fn(() => "blob:x");
  URL.revokeObjectURL = vi.fn();
  vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(() => {});
});

describe("DesktopReviewRow", () => {
  it("with no request offers Request download and the size, and keeps its test id", () => {
    renderRow();
    const li = screen.getByTestId("desktop-download");
    expect(li.textContent).toContain(file.name);
    expect(li.textContent).toContain("18.4 MB");
    expect(screen.getByRole("button", { name: C.requestDownload })).toBeTruthy();
    expect(screen.queryByText(C.stateWaiting)).toBeNull();
  });

  it("waiting shows the chip, the asked line and the deciders, and no button", () => {
    renderRow(row({ deciders: ["owner", "group:finance"] }));
    expect(screen.getByText(C.stateWaiting)).toBeTruthy();
    expect(screen.getByText(/Asked \d\d:\d\d · sha256 9f2c…41ab/)).toBeTruthy();
    expect(screen.getByText(DECIDERS_INBOX.DECIDED_BY_LABEL)).toBeTruthy();
    expect(screen.getByTestId("desktop-download").textContent).toContain(
      `${DECIDERS_INBOX.OWNER_SELF} or ${DECIDERS_INBOX.GROUP("finance")}`,
    );
    expect(screen.queryByRole("button")).toBeNull();
  });

  it("waiting with no deciders says a security admin decides", () => {
    renderRow(row({}));
    expect(screen.getByTestId("desktop-download").textContent).toContain(`${DECIDERS_INBOX.DECIDED_BY_LABEL} ${DECIDERS_INBOX.SECURITY}`);
  });

  it("approved offers Download approved, which downloads with the approval id", async () => {
    downloadMock.mockResolvedValue({ blob: new Blob(["x"]), sha256: SHA });
    const { onRefused } = renderRow(row({ state: "APPROVED", decided_by: "lee.chen@acme.example", decided_at: aheadByHours(-0.5) }));
    expect(screen.getByText(C.stateApproved)).toBeTruthy();
    expect(screen.getByText(/Approved by lee\.chen@acme\.example · \d\d:\d\d/)).toBeTruthy();
    await userEvent.click(screen.getByRole("button", { name: C.downloadApproved }));
    expect(downloadMock).toHaveBeenCalledWith("r1", file.name, "ap_1");
    expect(await screen.findByText(/Downloaded \d\d:\d\d · sha256 9f2c…41ab/)).toBeTruthy();
    expect(onRefused).toHaveBeenCalled();
  });

  it("denied shows the decider's reason and offers Request download again", () => {
    renderRow(row({ state: "DENIED", decided_by: "sam.okafor@acme.example", reason: "Customer records leave through the data team.", decided_at: aheadByHours(-1) }));
    expect(screen.getByText(C.stateDenied)).toBeTruthy();
    expect(screen.getByText(/sam\.okafor@acme\.example: “Customer records leave through the data team\.” · \d\d:\d\d/)).toBeTruthy();
    expect(screen.getByRole("button", { name: C.requestDownload })).toBeTruthy();
  });

  it("a released approval reads as downloaded and offers Request download again", () => {
    renderRow(row({ state: "APPROVED", minted_jti: "jti-1", decided_at: aheadByHours(-1) }));
    expect(screen.getByText(/Downloaded \d\d:\d\d · sha256 9f2c…41ab/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: C.downloadApproved })).toBeNull();
    expect(screen.getByRole("button", { name: C.requestDownload })).toBeTruthy();
  });

  it("expired says nobody decided it and offers Request download again", () => {
    renderRow(row({ state: "EXPIRED", decided_at: aheadByHours(-2) }));
    expect(screen.getByText(C.stateExpired)).toBeTruthy();
    expect(screen.getByText(/Nobody decided it in time · \d\d:\d\d/)).toBeTruthy();
    expect(screen.getByRole("button", { name: C.requestDownload })).toBeTruthy();
  });

  it("while asking shows Asking… on a disabled button, then hands up the pending row", async () => {
    let resolve!: (r: ReturnType<typeof makeApproval>) => void;
    reviewMock.mockReturnValue(new Promise((r) => (resolve = r)));
    const { onAsked } = renderRow();
    await userEvent.click(screen.getByRole("button", { name: C.requestDownload }));
    const busy = screen.getByRole("button", { name: C.asking }) as HTMLButtonElement;
    expect(busy.disabled).toBe(true);
    expect(reviewMock).toHaveBeenCalledWith("r1", file.name);
    const pending = row({});
    resolve(pending);
    await vi.waitFor(() => expect(onAsked).toHaveBeenCalledWith(pending));
  });

  it("a refused ask prints the server's sentence under the row and keeps the button", async () => {
    reviewMock.mockRejectedValue(new HttpError(503, "the download's review could not be raised.", "desktop_review_unavailable"));
    const { onRefused } = renderRow();
    await userEvent.click(screen.getByRole("button", { name: C.requestDownload }));
    expect(await screen.findByText(`${C.notAsked}: the download's review could not be raised.`)).toBeTruthy();
    expect(screen.getByRole("button", { name: C.requestDownload })).toBeTruthy();
    expect(onRefused).not.toHaveBeenCalled();
  });
});
