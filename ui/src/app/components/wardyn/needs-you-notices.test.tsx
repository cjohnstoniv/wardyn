/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The toast a takeover request raises offers Open run (a background run's too), and a hold on the person's own run
// raises "<run> is waiting for approval: <host>" with Review.
import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { ApprovalRequest, RunDetail } from "../../lib/types";

const { listApprovals, getRun, toast } = vi.hoisted(() => ({ listApprovals: vi.fn(), getRun: vi.fn(), toast: vi.fn() }));
vi.mock("sonner", () => ({ toast }));
vi.mock("../../lib/api/approvals", () => ({ approvals: { listApprovals } }));
vi.mock("../../lib/api/runs", () => ({ runs: { getRun } }));
vi.mock("./operator-context", () => ({ usePrincipal: () => "dev-sub" }));

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

const hold = {
  id: "ap-2",
  run_id: "run-1",
  kind: "egress_domain",
  requested_scope: { host: "api.example.com", mode: "wait_for_review" },
  state: "PENDING",
  held: true,
  requested_at: new Date().toISOString(),
} as unknown as ApprovalRequest;

async function mountWith(rows: ApprovalRequest[], run: Partial<RunDetail>) {
  listApprovals.mockResolvedValue(rows);
  getRun.mockResolvedValue({ id: "run-1", ...run });
  render(
    <MemoryRouter>
      <NeedsYouNotices />
    </MemoryRouter>,
  );
}

describe("NeedsYouNotices", () => {
  it("a run with a console gets an Open run action", async () => {
    expect((await toastOptions({ experience: "interactive" })).action?.label).toBe("Open run");
  });

  it("a background run gets the notice with the same Open run action", async () => {
    expect((await toastOptions({ experience: "background" })).action?.label).toBe(NEEDS_YOU.toast.openRun);
  });

  it("a hold on the person's own run raises a toast naming the host, with Review", async () => {
    await mountWith([hold], { created_by: "dev-sub", name: "build" } as Partial<RunDetail>);
    await waitFor(() => expect(toast).toHaveBeenCalled());
    const [title, opts] = toast.mock.calls[0] as [string, { action: { label: string } }];
    expect(title).toMatch(/ is waiting for approval: api\.example\.com$/);
    expect(opts.action.label).toBe("Review");
  });

  it("a hold on someone else's run, or one that is not held, raises nothing", async () => {
    await mountWith([hold], { created_by: "someone-else" });
    await waitFor(() => expect(getRun).toHaveBeenCalled());
    expect(toast).not.toHaveBeenCalled();
    window.sessionStorage.clear();
    await mountWith([{ ...hold, id: "ap-3", held: false }], { created_by: "dev-sub" });
    await new Promise((r) => setTimeout(r, 20));
    expect(toast).not.toHaveBeenCalled();
  });

  it("fetches a hold on someone else's run once, not on every poll", async () => {
    vi.useFakeTimers();
    try {
      await mountWith([hold], { created_by: "someone-else" });
      await vi.advanceTimersByTimeAsync(0);
      expect(getRun).toHaveBeenCalledTimes(1);
      await vi.advanceTimersByTimeAsync(15_000);
      expect(listApprovals).toHaveBeenCalledTimes(2);
      expect(getRun).toHaveBeenCalledTimes(1);
      expect(toast).not.toHaveBeenCalled();
    } finally {
      vi.useRealTimers();
    }
  });
});
