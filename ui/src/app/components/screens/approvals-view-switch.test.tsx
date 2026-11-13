/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A switch between the User and Admin view keeps this screen mounted and asks for the lists again in the new view. A
// read asked for the view left, answering after the switch, must not mark a list ready or failed in the new one.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter, useNavigate } from "react-router-dom";
import type { ApprovalRequest } from "../../lib/types";

vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn() } }));
type View = "user" | "admin";
const approveMock = vi.fn<() => Promise<unknown>>();
const pendingFor = { user: vi.fn<() => Promise<ApprovalRequest[]>>(), admin: vi.fn<() => Promise<ApprovalRequest[]>>() };
const decidedFor = { user: vi.fn<(state: string) => Promise<ApprovalRequest[]>>(), admin: vi.fn<(state: string) => Promise<ApprovalRequest[]>>() };
vi.mock("../../lib/api/approvals", () => ({
  approvals: {
    listApprovals: (state: string, _cursor: string, view: View) => (state === "PENDING" ? pendingFor[view]() : decidedFor[view](state)),
    knowsDecidableBy: async () => false,
    listDecidableByMe: async () => ({ rows: [], truncated: false }),
    approve: () => approveMock(),
    deny: vi.fn(),
  },
}));
vi.mock("../../lib/api/governance-requests", () => ({ requests: { list: async () => null } }));
vi.mock("../../lib/api/permissions", () => ({
  permissions: {
    getMyCapabilities: () =>
      Promise.resolve({ grants: [], enforcement: {}, session_groups: [], groups_snapshot_stale: false, kinds_version: 1 }),
  },
}));
vi.mock("../../lib/api/runs", () => ({ runs: { getRun: () => Promise.reject(new Error("unused")) } }));

import { ApprovalsScreen } from "./approvals";

const NOW = new Date().toISOString();
const egress = (id: string, extra: Partial<ApprovalRequest> = {}): ApprovalRequest => ({
  id,
  run_id: "4f2a91c0-1111-2222-3333-444455556666",
  kind: "egress_domain",
  requested_scope: { host: `${id}.example.com` },
  state: "PENDING",
  requested_at: NOW,
  ...extra,
});
const push = (id: string): ApprovalRequest => ({
  id,
  run_id: "4f2a91c0-1111-2222-3333-444455556666",
  kind: "push_content",
  requested_scope: { repo: "github.com/acme/widgets", branch: "refs/heads/feature/x", acts_as: "github_token:11111111-1111-1111-1111-111111111111", paths: ["a.txt"], paths_total: 1, commits: ["deadbeef".repeat(5)], paths_digest: "a".repeat(64), acts_as_kind: "github_app", acts_as_label: "dana@acme.example" },
  state: "PENDING",
  requested_at: NOW,
});
const later = <T,>() => {
  let answer: (v: T) => void = () => {};
  let fail: (e: Error) => void = () => {};
  const promise = new Promise<T>((done, no) => ((answer = done), (fail = no)));
  return { promise, answer, fail };
};
const flush = (ms = 0) =>
  act(async () => {
    await vi.advanceTimersByTimeAsync(ms);
  });

// The view switch the console's header makes: the same screen, another path.
let go: (path: string) => void = () => {};
function Nav() {
  const navigate = useNavigate();
  go = (path) => void navigate(path);
  return null;
}
async function mount(url: string) {
  render(
    <MemoryRouter initialEntries={[url]}>
      <ApprovalsScreen />
      <Nav />
    </MemoryRouter>,
  );
  await flush();
}
async function switchTo(path: string) {
  await act(async () => go(path));
  await flush();
}
const retry = () => screen.queryByRole("button", { name: /retry/i });
// The empty queue and the empty history each offer the other tab: a list still loading draws neither.
const empty = () => screen.queryByRole("button", { name: /^(see decided|back to pending)$/i });

beforeEach(() => {
  vi.useFakeTimers();
  for (const v of ["user", "admin"] as const) {
    pendingFor[v].mockReset().mockResolvedValue([]);
    decidedFor[v].mockReset().mockResolvedValue([]);
  }
});
afterEach(() => vi.useRealTimers());

describe("ApprovalsScreen: a read for the view left answers after a switch", () => {
  it("a User poll read does not show its rows, or the ones shown before, as the Admin queue", async () => {
    const poll = later<ApprovalRequest[]>();
    const admin = later<ApprovalRequest[]>();
    pendingFor.user.mockResolvedValueOnce([egress("u0")]).mockReturnValueOnce(poll.promise);
    pendingFor.admin.mockReturnValueOnce(admin.promise);
    await mount("/approvals");
    expect(screen.getByText("Reach u0.example.com")).toBeInTheDocument();
    await flush(10_000);
    expect(pendingFor.user).toHaveBeenCalledTimes(2);
    await switchTo("/admin/approvals");
    await act(async () => poll.answer([egress("u1")]));
    await flush();
    expect(screen.queryByText("Reach u1.example.com")).toBeNull();
    expect(screen.queryByText("Reach u0.example.com")).toBeNull();
    await act(async () => admin.answer([egress("a1")]));
    await flush();
    expect(screen.getByText("Reach a1.example.com")).toBeInTheDocument();
  });

  it("the User view's first read, answered or failed after the switch, marks the Admin queue neither ready nor failed", async () => {
    for (const ends of ["answers", "fails"] as const) {
      const user = later<ApprovalRequest[]>();
      const admin = later<ApprovalRequest[]>();
      pendingFor.user.mockReturnValueOnce(user.promise);
      pendingFor.admin.mockReturnValueOnce(admin.promise);
      await mount("/approvals");
      await switchTo("/admin/approvals");
      await act(async () => (ends === "answers" ? user.answer([egress("u1")]) : user.fail(new Error("offline"))));
      await flush();
      expect(screen.queryByText("Reach u1.example.com"), ends).toBeNull();
      expect(retry(), ends).toBeNull();
      expect(empty(), ends).toBeNull();
      await act(async () => admin.answer([egress("a1")]));
      await flush();
      expect(screen.getByText("Reach a1.example.com")).toBeInTheDocument();
      cleanup();
    }
  });

  it("the User view's decided read, answered or failed after the switch, marks the Admin history neither ready nor failed", async () => {
    for (const ends of ["answers", "fails"] as const) {
      const user = later<ApprovalRequest[]>();
      const admin = later<ApprovalRequest[]>();
      decidedFor.user.mockReset().mockImplementation((state) => user.promise.then((rows) => (state === "APPROVED" ? rows : [])));
      decidedFor.admin.mockReset().mockImplementation((state) => admin.promise.then((rows) => (state === "APPROVED" ? rows : [])));
      await mount("/approvals?tab=decided");
      await switchTo("/admin/approvals?tab=decided");
      await act(async () => (ends === "answers" ? user.answer([egress("du", { state: "APPROVED", decided_at: NOW })]) : user.fail(new Error("offline"))));
      await flush();
      expect(screen.queryByText("Reach du.example.com"), ends).toBeNull();
      expect(retry(), ends).toBeNull();
      expect(empty(), ends).toBeNull();
      await act(async () => admin.answer([egress("da", { state: "APPROVED", decided_at: NOW })]));
      await flush();
      expect(screen.getByText("Reach da.example.com")).toBeInTheDocument();
      cleanup();
    }
  });

  it("a User poll read of the decided history does not show the rows shown before as the Admin history", async () => {
    const poll = later<ApprovalRequest[]>();
    const admin = later<ApprovalRequest[]>();
    const row = (id: string) => [egress(id, { state: "APPROVED", decided_at: NOW })];
    let reads = 0;
    decidedFor.user.mockReset().mockImplementation((state) => (state !== "APPROVED" ? Promise.resolve([]) : reads++ === 0 ? Promise.resolve(row("d0")) : poll.promise));
    decidedFor.admin.mockReset().mockImplementation((state) => (state === "APPROVED" ? admin.promise : Promise.resolve([])));
    await mount("/approvals?tab=decided");
    expect(screen.getByText("Reach d0.example.com")).toBeInTheDocument();
    await flush(10_000);
    expect(reads).toBe(2);
    await switchTo("/admin/approvals?tab=decided");
    await act(async () => poll.answer(row("d1")));
    await flush();
    expect(screen.queryByText("Reach d1.example.com")).toBeNull();
    expect(screen.queryByText("Reach d0.example.com")).toBeNull();
    expect(empty()).toBeNull();
    await act(async () => admin.answer(row("da")));
    await flush();
    expect(screen.getByText("Reach da.example.com")).toBeInTheDocument();
  });

  // A card's own Approve opens no dialog, so the switch can come while the decision is out; its refresh then asks in
  // the view left, after the new view's own read has landed.
  it("a decision sent before the switch does not draw the User queue or history as the Admin one when it refreshes", async () => {
    for (const tab of ["pending", "decided"] as const) {
      const approve = later<unknown>();
      approveMock.mockReset().mockReturnValueOnce(approve.promise);
      pendingFor.user.mockReset().mockResolvedValueOnce([push("p1")]).mockResolvedValue([egress("u9")]);
      pendingFor.admin.mockResolvedValue([egress("a1")]);
      decidedFor.user.mockReset().mockImplementation(async (state) => (state === "APPROVED" ? [egress("du", { state: "APPROVED", decided_at: NOW })] : []));
      decidedFor.admin.mockReset().mockImplementation(async (state) => (state === "APPROVED" ? [egress("da", { state: "APPROVED", decided_at: NOW })] : []));
      await mount("/approvals");
      fireEvent.click(within(screen.getByTestId("push-content-card")).getByRole("button", { name: /^Approve$/ }));
      await flush();
      await switchTo("/admin/approvals");
      if (tab === "decided") {
        fireEvent.mouseDown(screen.getByRole("tab", { name: /decided/i }));
        await flush();
      }
      const [mine, theirs] = tab === "pending" ? ["Reach a1.example.com", "Reach u9.example.com"] : ["Reach da.example.com", "Reach du.example.com"];
      expect(screen.getByText(mine), tab).toBeInTheDocument();
      await act(async () => approve.answer({}));
      await flush();
      expect(pendingFor.user, tab).toHaveBeenCalledTimes(2);
      expect(screen.queryByText(theirs), tab).toBeNull();
      expect(screen.getByText(mine), tab).toBeInTheDocument();
      cleanup();
    }
  });
});
