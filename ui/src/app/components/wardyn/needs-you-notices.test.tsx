/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The toast a takeover request raises offers Take over, except for a background run, which has no console to take over.
import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { ApprovalRequest, RunDetail } from "../../lib/types";

const { listApprovals, getRun, toast } = vi.hoisted(() => ({ listApprovals: vi.fn(), getRun: vi.fn(), toast: vi.fn() }));
vi.mock("sonner", () => ({ toast }));
vi.mock("../../lib/api/approvals", () => ({ approvals: { listApprovals } }));
vi.mock("../../lib/api/runs", () => ({ runs: { getRun } }));

import { NeedsYouNotices } from "./needs-you-notices";
import { NEEDS_YOU } from "./copy/needs-you";

const ap = {
  id: "ap-1",
  run_id: "run-1",
  kind: "human_takeover",
  requested_scope: { reason: "Stuck on a sign-in." },
  state: "PENDING",
  requested_at: new Date().toISOString(),
} as unknown as ApprovalRequest;

beforeEach(() => {
  vi.clearAllMocks();
  window.sessionStorage.clear();
  listApprovals.mockResolvedValue([ap]);
});

async function toastOptions(run: Partial<RunDetail>) {
  getRun.mockResolvedValue({ id: "run-1", ...run });
  render(
    <MemoryRouter>
      <NeedsYouNotices />
    </MemoryRouter>,
  );
  await waitFor(() => expect(toast).toHaveBeenCalled());
  return toast.mock.calls[0][1] as { action?: { label: string } };
}

describe("NeedsYouNotices", () => {
  it("a run with a console gets a Take over action", async () => {
    expect((await toastOptions({ experience: "interactive" })).action?.label).toBe(NEEDS_YOU.banner.takeOver);
  });

  it("a background run gets the notice with no Take over action", async () => {
    expect((await toastOptions({ experience: "background" })).action).toBeUndefined();
  });
});
