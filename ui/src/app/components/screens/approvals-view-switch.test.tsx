/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A switch between the User and Admin view keeps this screen mounted and asks for the lists again in the new view. A
// read asked for the view left, answering after the switch, must not mark a list ready or failed in the new one.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter, useNavigate } from "react-router-dom";
import { toast } from "sonner";
import { HttpError } from "../../lib/api/core";
import { APPROVALS } from "../../lib/approvals-copy";
import type { ApprovalRequest } from "../../lib/types";

vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn() } }));
type View = "user" | "admin";
const approveMock = vi.fn<() => Promise<unknown>>();
const declineMock = vi.fn<() => Promise<unknown>>();
const pendingFor = { user: vi.fn<() => Promise<ApprovalRequest[]>>(), admin: vi.fn<() => Promise<ApprovalRequest[]>>() };
const decidedFor = { user: vi.fn<(state: string) => Promise<ApprovalRequest[]>>(), admin: vi.fn<(state: string) => Promise<ApprovalRequest[]>>() };
vi.mock("../../lib/api/approvals", () => ({
  approvals: {
    listApprovals: (state: string, _cursor: string, view: View) => (state === "PENDING" ? pendingFor[view]() : decidedFor[view](state)),
    knowsDecidableBy: async () => false,
    listDecidableByMe: async () => ({ rows: [], truncated: false }),
    approve: () => approveMock(),
    deny: vi.fn(),
    decline: () => declineMock(),
  },
}));
const elevationList = vi.fn<() => Promise<unknown>>(async () => null);
const elevationApprove = vi.fn<() => Promise<unknown>>();
vi.mock("../../lib/api/governance-requests", () => ({ requests: { list: () => elevationList(), approve: () => elevationApprove() } }));
vi.mock("../../lib/api/permissions", () => ({
  permissions: {
    getMyCapabilities: () =>
      Promise.resolve({ grants: [], enforcement: {}, session_groups: [], groups_snapshot_stale: false, kinds_version: 1 }),
  },
}));
vi.mock("../../lib/api/runs", () => ({ runs: { getRun: () => Promise.reject(new Error("unused")) } }));

import { OperatorProvider } from "../wardyn/operator-context";
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
const takeover = (id: string) => egress(id, { kind: "human_takeover", requested_scope: { reason: "stuck on a prompt" } });
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
async function mount(url: string, securityOperator = true) {
  render(
    <OperatorProvider operator={false} securityOperator={securityOperator}>
      <MemoryRouter initialEntries={[url]}>
        <ApprovalsScreen />
        <Nav />
      </MemoryRouter>
    </OperatorProvider>,
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
  elevationList.mockReset().mockResolvedValue(null);
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
  // the view shown, never the view left, so no User row can reach the Admin queue or history.
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
      const mine = tab === "pending" ? "Reach a1.example.com" : "Reach da.example.com";
      expect(screen.getByText(mine), tab).toBeInTheDocument();
      const adminReads = pendingFor.admin.mock.calls.length;
      await act(async () => approve.answer({}));
      await flush();
      expect(pendingFor.user, tab).toHaveBeenCalledTimes(1);
      expect(pendingFor.admin.mock.calls.length, tab).toBe(adminReads + 1);
      // The User mocks' second answer (u9, du) is never read, so no assertion on those rows could fail: the call counts
      // above are what show the refresh did not ask the view left.
      expect(screen.getByText(mine), tab).toBeInTheDocument();
      cleanup();
    }
  });

  // The new view's own read can land while the decision is out and still list the request as pending; the refresh after
  // the answer is then the only thing that takes its Approve and Deny away, so it asks in the view shown.
  it("a request decided before the switch has no Approve or Deny in the Admin queue once the decision answers", async () => {
    const approve = later<unknown>();
    approveMock.mockReset().mockReturnValueOnce(approve.promise);
    pendingFor.user.mockReset().mockResolvedValue([push("p1")]);
    pendingFor.admin.mockReset().mockResolvedValueOnce([push("p1")]).mockResolvedValue([]);
    await mount("/approvals");
    fireEvent.click(within(screen.getByTestId("push-content-card")).getByRole("button", { name: /^Approve$/ }));
    await flush();
    await switchTo("/admin/approvals");
    expect(within(screen.getByTestId("push-content-card")).getByRole("button", { name: /^Approve$/ })).toBeInTheDocument();
    await act(async () => approve.answer({}));
    await flush();
    expect(screen.queryByTestId("push-content-card")).toBeNull();
    expect(pendingFor.admin).toHaveBeenCalledTimes(2);
  });

  it("a Needs you request declined before the switch has no Decline in the Admin queue once the decline answers", async () => {
    const decline = later<unknown>();
    declineMock.mockReset().mockReturnValueOnce(decline.promise);
    pendingFor.user.mockReset().mockResolvedValue([takeover("t1")]);
    pendingFor.admin.mockReset().mockResolvedValueOnce([takeover("t1")]).mockResolvedValue([]);
    await mount("/approvals");
    fireEvent.click(within(screen.getByTestId("takeover-card")).getByRole("button", { name: /^Decline$/ }));
    await flush();
    await switchTo("/admin/approvals");
    expect(screen.getByTestId("takeover-card")).toBeInTheDocument();
    await act(async () => decline.answer({}));
    await flush();
    expect(screen.queryByTestId("takeover-card")).toBeNull();
    expect(pendingFor.user).toHaveBeenCalledTimes(1);
  });

  // A decider's elevation card refreshes through the same ref: the pending read after its answer is in the view shown.
  it("an elevation request decided before the switch refreshes the Admin queue, not the User one", async () => {
    const approve = later<unknown>();
    elevationApprove.mockReset().mockReturnValueOnce(approve.promise);
    elevationList.mockReset().mockResolvedValue({
      requests: [
        { id: "e1", kind: "elevation", target_kind: "governance_elevation", op: "create", target_key: "u1", state: "pending", unit_id: "u1", unit_name: "Payments", duration: "4h", proposed_by: "dana@acme.example", proposed_at: NOW, expires_at: NOW, diff: { before: null, after: null, changed: [] }, mine: false, decidable: true, deciders: [{ kind: "security" }], note: { reason: "ship it" } },
      ],
      offered: [],
      image_offered: false,
    });
    await mount("/approvals", false);
    fireEvent.mouseDown(screen.getByRole("tab", { name: /waiting on you/i }));
    await flush();
    fireEvent.click(screen.getByRole("button", { name: /^approve$/i }));
    await flush();
    expect(elevationApprove).toHaveBeenCalledTimes(1);
    await switchTo("/admin/approvals");
    const [userReads, adminReads] = [pendingFor.user.mock.calls.length, pendingFor.admin.mock.calls.length];
    await act(async () => approve.answer({}));
    await flush();
    expect(pendingFor.user.mock.calls.length).toBe(userReads);
    expect(pendingFor.admin.mock.calls.length).toBe(adminReads + 1);
  });

  // The reason dialog is modal, but the browser's Back and Forward still switch the view under it.
  it("a request approved from the reason dialog before the switch is gone from the Admin queue once the decision answers", async () => {
    const approve = later<unknown>();
    approveMock.mockReset().mockReturnValueOnce(approve.promise);
    pendingFor.user.mockReset().mockResolvedValue([egress("p1")]);
    pendingFor.admin.mockReset().mockResolvedValueOnce([egress("p1")]).mockResolvedValue([]);
    await mount("/approvals");
    fireEvent.click(screen.getByRole("button", { name: /^approve$/i }));
    await flush();
    fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: /^approve$/i }));
    await flush();
    await switchTo("/admin/approvals");
    expect(screen.getByText("Reach p1.example.com")).toBeInTheDocument();
    await act(async () => approve.answer({}));
    await flush();
    expect(screen.queryByText("Reach p1.example.com")).toBeNull();
    expect(pendingFor.user).toHaveBeenCalledTimes(1);
  });
});

// Someone else decided the request first, so the server refuses the click: 409, reason approval_already_decided. The toast
// says so in the server's words, and the stale card goes with the read that follows at once, not at the next poll.
describe("ApprovalsScreen: a decision the server refuses as already decided", () => {
  const refused = (sentence = "approval already decided") => new HttpError(409, sentence, "approval_already_decided");

  it("a card's own Approve stays busy while the queue is read again, and the card is gone when that read answers", async () => {
    const again = later<ApprovalRequest[]>();
    approveMock.mockReset().mockRejectedValueOnce(refused());
    pendingFor.user.mockReset().mockResolvedValueOnce([push("p1")]).mockReturnValueOnce(again.promise);
    vi.mocked(toast.error).mockClear();
    await mount("/approvals");
    fireEvent.click(within(screen.getByTestId("push-content-card")).getByRole("button", { name: /^Approve$/ }));
    await flush();
    expect(toast.error).toHaveBeenCalledWith(APPROVALS.TOAST_APPROVE_FAILED, { description: "approval already decided" });
    expect(pendingFor.user).toHaveBeenCalledTimes(2);
    expect(within(screen.getByTestId("push-content-card")).getByRole("button", { name: /^Approve$/ })).toBeDisabled();
    await act(async () => again.answer([]));
    await flush();
    expect(screen.queryByTestId("push-content-card")).toBeNull();
  });

  it("the reason dialog closes with the card", async () => {
    approveMock.mockReset().mockRejectedValueOnce(refused());
    pendingFor.user.mockReset().mockResolvedValueOnce([egress("p1")]).mockResolvedValue([]);
    await mount("/approvals");
    fireEvent.click(screen.getByRole("button", { name: /^approve$/i }));
    await flush();
    fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: /^approve$/i }));
    await flush();
    expect(pendingFor.user).toHaveBeenCalledTimes(2);
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(screen.queryByText("Reach p1.example.com")).toBeNull();
  });

  it("a Decline of a Needs you request that is no longer open takes its card away", async () => {
    declineMock.mockReset().mockRejectedValueOnce(refused("this takeover request is no longer open"));
    pendingFor.user.mockReset().mockResolvedValueOnce([takeover("t1")]).mockResolvedValue([]);
    await mount("/approvals");
    fireEvent.click(within(screen.getByTestId("takeover-card")).getByRole("button", { name: /^Decline$/ }));
    await flush();
    expect(pendingFor.user).toHaveBeenCalledTimes(2);
    expect(screen.queryByTestId("takeover-card")).toBeNull();
  });

  it("any other refusal leaves the card and reads nothing", async () => {
    approveMock.mockReset().mockRejectedValueOnce(new HttpError(500, "internal error"));
    pendingFor.user.mockReset().mockResolvedValue([push("p1")]);
    await mount("/approvals");
    fireEvent.click(within(screen.getByTestId("push-content-card")).getByRole("button", { name: /^Approve$/ }));
    await flush();
    expect(pendingFor.user).toHaveBeenCalledTimes(1);
    expect(within(screen.getByTestId("push-content-card")).getByRole("button", { name: /^Approve$/ })).toBeEnabled();
  });
});

// A request with a decision out is held in every view: the card's own busy state is lost when a view switch remounts it,
// and is already clear when the read after a decision starts, so the screen holds the request from the click until that
// read has answered.
describe("ApprovalsScreen: a request with a decision out has no live buttons in any view", () => {
  const cardButtons = () => {
    const card = within(screen.getByTestId("push-content-card"));
    return [card.getByRole("button", { name: /^Approve$/ }), card.getByRole("button", { name: /^Deny$/ })];
  };

  it("a view switched to while the decision is out shows the card with both buttons off", async () => {
    const approve = later<unknown>();
    approveMock.mockReset().mockReturnValueOnce(approve.promise);
    pendingFor.user.mockReset().mockResolvedValue([push("p1")]);
    pendingFor.admin.mockReset().mockResolvedValueOnce([push("p1")]).mockResolvedValue([]);
    await mount("/approvals");
    fireEvent.click(cardButtons()[0]);
    await flush();
    await switchTo("/admin/approvals");
    for (const button of cardButtons()) expect(button).toBeDisabled();
    await act(async () => approve.answer({}));
    await flush();
    expect(screen.queryByTestId("push-content-card")).toBeNull();
  });

  it("after a decision that went through the card stays off until the read after it has answered", async () => {
    const again = later<ApprovalRequest[]>();
    approveMock.mockReset().mockResolvedValueOnce({});
    pendingFor.user.mockReset().mockResolvedValueOnce([push("p1")]).mockReturnValueOnce(again.promise);
    await mount("/approvals");
    fireEvent.click(cardButtons()[0]);
    await flush();
    expect(pendingFor.user).toHaveBeenCalledTimes(2);
    for (const button of cardButtons()) expect(button).toBeDisabled();
    await act(async () => again.answer([push("p1")]));
    await flush();
    for (const button of cardButtons()) expect(button).toBeEnabled();
  });

  it("a decision the server refuses for any other reason frees the card at once, and a second click goes out", async () => {
    approveMock.mockReset().mockRejectedValueOnce(new HttpError(500, "internal error")).mockResolvedValueOnce({});
    pendingFor.user.mockReset().mockResolvedValue([push("p1")]);
    await mount("/approvals");
    fireEvent.click(cardButtons()[0]);
    await flush();
    expect(pendingFor.user).toHaveBeenCalledTimes(1);
    for (const button of cardButtons()) expect(button).toBeEnabled();
    fireEvent.click(cardButtons()[0]);
    await flush();
    expect(approveMock).toHaveBeenCalledTimes(2);
  });
});

// The hold is the screen's, so it covers every way a decision goes out, not only a card's own Approve.
describe("ApprovalsScreen: every decision path holds its request, and a refusal frees it", () => {
  const approveOf = (i: number) => screen.getAllByRole("button", { name: /^approve$/i })[i];
  const inDialog = (name: RegExp) => within(screen.getByRole("dialog")).getByRole("button", { name });
  async function sendFromDialog(i = 0) {
    approveOf(i).focus();
    fireEvent.click(approveOf(i));
    await flush();
    fireEvent.click(inDialog(/^approve$/i));
    await flush();
  }

  it("a reason dialog closed while its decision is out leaves the card with both buttons off, until the decision is refused", async () => {
    const approve = later<unknown>();
    approveMock.mockReset().mockReturnValueOnce(approve.promise);
    pendingFor.user.mockReset().mockResolvedValue([egress("p1")]);
    await mount("/approvals");
    await sendFromDialog();
    fireEvent.click(inDialog(/^cancel$/i));
    await flush();
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(approveOf(0)).toBeDisabled();
    expect(screen.getByRole("button", { name: /^deny$/i })).toBeDisabled();
    await act(async () => approve.fail(new HttpError(500, "internal error")));
    await flush();
    expect(approveOf(0)).toBeEnabled();
  });

  it.each([
    ["goes through", (a: ReturnType<typeof later<unknown>>) => a.answer({})],
    ["is refused as already decided", (a: ReturnType<typeof later<unknown>>) => a.fail(new HttpError(409, "approval already decided", "approval_already_decided"))],
  ])("a decision that %s after its dialog was closed does not close the dialog opened since for another request", async (_name, settle) => {
    const approve = later<unknown>();
    approveMock.mockReset().mockReturnValueOnce(approve.promise);
    pendingFor.user.mockReset().mockResolvedValueOnce([egress("p1"), egress("p2")]).mockResolvedValue([egress("p2")]);
    await mount("/approvals");
    await sendFromDialog();
    fireEvent.click(inDialog(/^cancel$/i));
    await flush();
    fireEvent.click(approveOf(1));
    await flush();
    await act(async () => settle(approve));
    await flush();
    expect(screen.getByRole("dialog")).toBeInTheDocument();
    expect(screen.queryByText("Reach p1.example.com")).toBeNull();
  });

  it("a dialog decision that takes its card away leaves focus on the next card's Approve, not on the page heading", async () => {
    const again = later<ApprovalRequest[]>();
    approveMock.mockReset().mockResolvedValueOnce({});
    pendingFor.user.mockReset().mockResolvedValueOnce([egress("p1"), egress("p2")]).mockReturnValueOnce(again.promise);
    await mount("/approvals");
    await sendFromDialog();
    await flush(300);
    await act(async () => again.answer([egress("p2")]));
    await flush(300);
    expect(screen.queryByText("Reach p1.example.com")).toBeNull();
    expect(approveOf(0)).toHaveFocus();
  });

  it("a Needs you request declined before the switch has Decline off in the Admin queue, and a refused decline frees it", async () => {
    const decline = later<unknown>();
    declineMock.mockReset().mockReturnValueOnce(decline.promise);
    pendingFor.user.mockReset().mockResolvedValue([takeover("t1")]);
    pendingFor.admin.mockReset().mockResolvedValue([takeover("t1")]);
    const declineButton = () => within(screen.getByTestId("takeover-card")).getByRole("button", { name: /^Decline$/ });
    await mount("/approvals");
    fireEvent.click(declineButton());
    await flush();
    await switchTo("/admin/approvals");
    expect(declineButton()).toBeDisabled();
    await act(async () => decline.fail(new HttpError(500, "internal error")));
    await flush();
    expect(declineButton()).toBeEnabled();
  });

  it("a refusal that has its own toast frees the card too", async () => {
    approveMock.mockReset().mockRejectedValueOnce(new HttpError(403, "above the rule", "decision_scope_above_rule"));
    pendingFor.user.mockReset().mockResolvedValue([push("p1")]);
    await mount("/approvals");
    const approve = () => within(screen.getByTestId("push-content-card")).getByRole("button", { name: /^Approve$/ });
    fireEvent.click(approve());
    await flush();
    expect(approve()).toBeEnabled();
  });

  // A decider's elevation card sends its own decision and keeps its own busy state, which a remount loses.
  it("an elevation request with a decision out has both buttons off after leaving the tab and coming back, until the read after it answers", async () => {
    const approve = later<unknown>();
    const again = later<unknown>();
    const list = (requests: unknown[]) => ({ requests, offered: [], image_offered: false });
    const e1 = { id: "e1", kind: "elevation", target_kind: "governance_elevation", op: "create", target_key: "u1", state: "pending", unit_id: "u1", unit_name: "Payments", duration: "4h", proposed_by: "dana@acme.example", proposed_at: NOW, expires_at: NOW, diff: { before: null, after: null, changed: [] }, mine: false, decidable: true, deciders: [{ kind: "security" }], note: { reason: "ship it" } };
    elevationApprove.mockReset().mockReturnValueOnce(approve.promise);
    elevationList.mockReset().mockResolvedValueOnce(list([e1])).mockReturnValueOnce(again.promise).mockResolvedValue(list([]));
    const tab = async (name: RegExp) => {
      fireEvent.mouseDown(screen.getByRole("tab", { name }));
      await flush();
    };
    const buttons = () => [screen.getByRole("button", { name: /^approve$/i }), screen.getByRole("button", { name: /^deny$/i })];
    await mount("/approvals", false);
    await tab(/waiting on you/i);
    fireEvent.click(buttons()[0]);
    await flush();
    await tab(/^pending/i);
    await tab(/waiting on you/i);
    for (const button of buttons()) expect(button).toBeDisabled();
    await act(async () => approve.answer({}));
    await flush();
    expect(elevationList).toHaveBeenCalledTimes(2);
    for (const button of buttons()) expect(button).toBeDisabled();
    await act(async () => again.answer(list([])));
    await flush();
    expect(screen.queryByRole("button", { name: /^approve$/i })).toBeNull();
  });

  // Tailwind writes space-y with no specificity, so any margin class on a row wins over it and the cards touch.
  it("a card's row carries no margin class, so the list's gap between cards holds", async () => {
    pendingFor.user.mockReset().mockResolvedValue([push("p1")]);
    await mount("/approvals");
    expect(screen.getByTestId("push-content-card").closest("fieldset")?.className).not.toMatch(/(^|\s)-?m[trblxy]?-/);
  });
});
