/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The demo's inline audit panel claims "live" only while its poll is answering.
import { act, cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { audit } from "../../../lib/api/audit";
import { DemoAuditPanel } from "./demo-runner";

vi.mock("../../../lib/api/audit", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../../../lib/api/audit")>()),
  audit: { listAudit: vi.fn() },
}));
vi.mock("../../attach-terminal", () => ({ AttachTerminal: () => null }));
vi.mock("../../wardyn/live-approvals", () => ({ LiveApprovals: () => null }));

const listAudit = vi.mocked(audit.listAudit);
const advance = (ms: number) => act(() => vi.advanceTimersByTimeAsync(ms));

beforeEach(() => {
  vi.useFakeTimers();
  listAudit.mockReset();
});
afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

describe("DemoAuditPanel (#2503)", () => {
  it("drops the live chip while the poll is failing and restores it once a poll answers", async () => {
    listAudit.mockResolvedValueOnce([]).mockRejectedValueOnce(new Error("503")).mockResolvedValue([]);
    render(<DemoAuditPanel runId="run-1" section="egress" />);
    await advance(0);
    expect(screen.getByText("live")).toBeInTheDocument();

    await advance(2000);
    expect(screen.queryByText("live")).not.toBeInTheDocument();

    await advance(2000);
    expect(screen.getByText("live")).toBeInTheDocument();
  });
});
