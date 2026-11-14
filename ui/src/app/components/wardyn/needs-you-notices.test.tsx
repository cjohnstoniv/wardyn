/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The toast a takeover request raises offers Open run (a background run's too), and a hold on the person's own run
// raises "<run> is waiting for approval: <host>" with Review. A notice belongs to the person signed in when it was
// read: a read that answers after a sign-out or an unconfirmed auth change shows nothing, and signing out closes them.
import { beforeEach, describe, expect, it, vi } from "vitest";
import { act, render, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { ApprovalRequest, RunDetail } from "../../lib/types";

const { listApprovals, getRun, toast, auth } = vi.hoisted(() => ({
  listApprovals: vi.fn(),
  getRun: vi.fn(),
  toast: Object.assign(vi.fn(), { dismiss: vi.fn() }),
  auth: { generation: 0 },
}));
vi.mock("sonner", () => ({ toast }));
vi.mock("../../lib/api/approvals", () => ({ approvals: { listApprovals } }));
vi.mock("../../lib/api/runs", () => ({ runs: { getRun } }));
vi.mock("../../lib/api/core", async (original) => ({
  ...(await original<typeof import("../../lib/api/core")>()),
  getAuthGeneration: () => auth.generation,
}));
vi.mock("./operator-context", () => ({ useRequestIdentity: () => ({ principal: "dev-sub", authGeneration: 0 }) }));

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
  auth.generation = 0;
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

  it("a run read that answers after sign-out raises nothing", async () => {
    let answer: (run: Partial<RunDetail>) => void = () => undefined;
    getRun.mockReturnValue(new Promise((resolve) => (answer = resolve)));
    const { unmount } = render(
      <MemoryRouter>
        <NeedsYouNotices />
      </MemoryRouter>,
    );
    await waitFor(() => expect(getRun).toHaveBeenCalled());
    unmount();
    await act(async () => answer({ id: "run-1" }));
    expect(toast).not.toHaveBeenCalled();
  });

  it("a run read that answers after an auth change not yet confirmed as the same person raises nothing", async () => {
    let answer: (run: Partial<RunDetail>) => void = () => undefined;
    getRun.mockReturnValue(new Promise((resolve) => (answer = resolve)));
    render(
      <MemoryRouter>
        <NeedsYouNotices />
      </MemoryRouter>,
    );
    await waitFor(() => expect(getRun).toHaveBeenCalled());
    auth.generation = 1;
    await act(async () => answer({ id: "run-1" }));
    expect(toast).not.toHaveBeenCalled();
  });

  it("signing out closes this person's notices, the browser one too, and no other toast", async () => {
    const close = vi.fn();
    vi.stubGlobal(
      "Notification",
      Object.assign(
        function FakeNotification() {
          return { close };
        },
        { permission: "granted" },
      ),
    );
    Object.defineProperty(document, "hidden", { configurable: true, get: () => true });
    try {
      getRun.mockResolvedValue({ id: "run-1" });
      const { unmount } = render(
        <MemoryRouter>
          <NeedsYouNotices />
        </MemoryRouter>,
      );
      await waitFor(() => expect(toast).toHaveBeenCalled());
      expect(toast.dismiss).not.toHaveBeenCalled();
      unmount();
      expect(toast.dismiss.mock.calls).toEqual([["needs-you-ap-1"]]);
      expect(close).toHaveBeenCalledTimes(1);
    } finally {
      vi.unstubAllGlobals();
      delete (document as { hidden?: boolean }).hidden;
    }
  });
});
