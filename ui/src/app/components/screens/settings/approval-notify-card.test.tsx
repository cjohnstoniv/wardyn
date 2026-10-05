/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import type { ApprovalNotifyStatus } from "../../../lib/api/approval-notify";
import { APPROVAL_NOTIFY as COPY } from "../../wardyn/copy/approval-notify";
import { expandCard } from "../../../lib/test-dom";

// notify-e4 (packet M6 S3): the read-only Approval notifications card.

const getStatusMock = vi.fn();
vi.mock("../../../lib/api/approval-notify", () => ({
  approvalNotify: { getStatus: (...a: unknown[]) => getStatusMock(...a) },
}));

import { ApprovalNotifyCard } from "./approval-notify-card";

const ago = (ms: number) => new Date(Date.now() - ms).toISOString();

const status: ApprovalNotifyStatus = {
  channels: [
    { id: "sec-oncall", type: "slack", destination_host: "hooks.slack.com", last_success_at: ago(4 * 60_000), failed_last_hour: 0 },
    {
      id: "platform", type: "webhook", destination_host: "hooks.example.com",
      last_error: "http_status:503", last_error_at: ago(6 * 60_000), failed_last_hour: 4,
    },
  ],
};

beforeEach(() => {
  getStatusMock.mockReset();
});

describe("ApprovalNotifyCard", () => {
  it("summarises failures and renders each channel row with the M6 strings", async () => {
    getStatusMock.mockResolvedValue(status);
    render(<ApprovalNotifyCard />);
    await screen.findByText(COPY.SUMMARY_FAILED(2, 1));
    await expandCard(COPY.TITLE);

    expect(screen.getByText(`${COPY.LEDE[0]}`, { exact: false })).toBeInTheDocument();
    expect(screen.getByText(COPY.ENV_NAME)).toBeInTheDocument();
    for (const col of Object.values(COPY.COLS)) expect(screen.getByRole("columnheader", { name: col })).toBeInTheDocument();

    const good = within(screen.getByText("sec-oncall").closest("tr")!);
    expect(good.getByText("Slack")).toBeInTheDocument();
    expect(good.getByText("hooks.slack.com")).toBeInTheDocument();
    expect(good.getByText("4m ago")).toBeInTheDocument();
    expect(good.getByText(COPY.NONE)).toBeInTheDocument();

    const bad = within(screen.getByText("platform").closest("tr")!);
    expect(bad.getByText("Webhook")).toBeInTheDocument();
    expect(bad.getByText(COPY.NEVER)).toBeInTheDocument();
    expect(bad.getByText("http_status:503")).toBeInTheDocument();
    expect(bad.getByText("6m ago")).toBeInTheDocument();
    expect(bad.getByText("4")).toBeInTheDocument();

    expect(screen.getByText(COPY.AUDIT_ACTION)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /save|add|test|send/i })).not.toBeInTheDocument();
  });

  it("says all delivering when nothing failed", async () => {
    getStatusMock.mockResolvedValue({ channels: [status.channels[0]] } satisfies ApprovalNotifyStatus);
    render(<ApprovalNotifyCard />);
    expect(await screen.findByText(COPY.SUMMARY_OK(1))).toBeInTheDocument();
  });

  it("shows the not-set-up summary and body when no channel is configured", async () => {
    getStatusMock.mockResolvedValue({ channels: [] } satisfies ApprovalNotifyStatus);
    render(<ApprovalNotifyCard />);
    await screen.findByText(COPY.SUMMARY_OFF);
    await expandCard(COPY.TITLE);
    expect(screen.getByText(COPY.ENV_NAME)).toBeInTheDocument();
    expect(screen.getByText(COPY.OFF_BODY[1].trim(), { exact: false })).toBeInTheDocument();
    expect(screen.queryByRole("table")).not.toBeInTheDocument();
  });

  it("shows no summary and an error state when the read fails", async () => {
    getStatusMock.mockRejectedValue(new Error("boom"));
    render(<ApprovalNotifyCard />);
    await expandCard(COPY.TITLE);
    expect(await screen.findByText("Something went wrong")).toBeInTheDocument();
    expect(screen.queryByText(COPY.SUMMARY_OFF)).not.toBeInTheDocument();
  });
});
