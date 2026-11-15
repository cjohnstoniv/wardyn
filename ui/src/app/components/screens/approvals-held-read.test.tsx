/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A request with a decision out stays held until a read of its own list that began after the decision has landed.
// A failed read, a Waiting on you read that fails on its own, or an elevation read a newer one superseded leaves the
// list shown before the decision on screen, so none of them frees the card; a bound does.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { HttpError } from "../../lib/api/core";
import type { ApprovalRequest } from "../../lib/types";

vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn() } }));
let pendingFails = false;
let pendingRows: ApprovalRequest[] = [];
let knows = false;
let waitingFails = false;
let waitingRows: ApprovalRequest[] = [];
const approveMock = vi.fn<() => Promise<unknown>>();
const declineMock = vi.fn<() => Promise<unknown>>();
const pendingMock = vi.fn(() => (pendingFails ? Promise.reject(new Error("read failed")) : Promise.resolve(pendingRows)));
const waitingMock = vi.fn(() =>
  waitingFails ? Promise.reject(new Error("read failed")) : Promise.resolve({ rows: waitingRows, truncated: false }),
);
vi.mock("../../lib/api/approvals", () => ({
  approvals: {
    listApprovals: (state: string) => (state === "PENDING" ? pendingMock() : Promise.resolve([])),
    knowsDecidableBy: async () => knows,
    listDecidableByMe: () => waitingMock(),
    approve: () => approveMock(),
    deny: vi.fn(),
    decline: () => declineMock(),
  },
}));
const elevationList = vi.fn<() => Promise<unknown>>(async () => null);
const elevationApprove = vi.fn<() => Promise<unknown>>();
vi.mock("../../lib/api/governance-requests", () => ({
  requests: { list: () => elevationList(), approve: () => elevationApprove(), reject: vi.fn() },
}));
vi.mock("../../lib/api/governance", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../../lib/api/governance")>()),
  governance: { getGovernance: () => Promise.resolve({ unit_names: {}, profiles: [] }) },
}));
vi.mock("../../lib/api/permissions", () => ({
  permissions: {
    getMyCapabilities: () =>
      Promise.resolve({ grants: [], enforcement: {}, session_groups: [], groups_snapshot_stale: false, kinds_version: 1 }),
  },
}));
vi.mock("../../lib/api/runs", () => ({ runs: { getRun: () => Promise.reject(new Error("unused")) } }));

import { OperatorProvider } from "../wardyn/operator-context";
import { ApprovalsScreen } from "./approvals";
import { READ_HOLD_MS } from "./approvals-read-hold";

const NOW = new Date().toISOString();
const push = (id: string): ApprovalRequest => ({
  id,
  run_id: "4f2a91c0-1111-2222-3333-444455556666",
  kind: "push_content",
  requested_scope: { repo: "github.com/acme/widgets", branch: "refs/heads/feature/x", acts_as: "github_token:11111111-1111-1111-1111-111111111111", paths: ["a.txt"], paths_total: 1, commits: ["deadbeef".repeat(5)], paths_digest: "a".repeat(64), acts_as_kind: "github_app", acts_as_label: "dana@acme.example" },
  state: "PENDING",
  requested_at: NOW,
});
const egress = (id: string, extra: Partial<ApprovalRequest> = {}): ApprovalRequest => ({
  id,
  run_id: "4f2a91c0-1111-2222-3333-444455556666",
  kind: "egress_domain",
  requested_scope: { host: `${id}.example.com` },
  state: "PENDING",
  requested_at: NOW,
  ...extra,
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

async function mount(url: string, securityOperator = true) {
  render(
    <OperatorProvider operator={false} securityOperator={securityOperator}>
      <MemoryRouter initialEntries={[url]}>
        <ApprovalsScreen />
      </MemoryRouter>
    </OperatorProvider>,
  );
  await flush();
}
const cardButtons = () => {
  const card = within(screen.getByTestId("push-content-card"));
  return [card.getByRole("button", { name: /^Approve$/ }), card.getByRole("button", { name: /^Deny$/ })];
};
const list = (requests: unknown[]) => ({ requests, offered: [], image_offered: false });
const e1 = { id: "e1", kind: "elevation", target_kind: "governance_elevation", op: "create", target_key: "u1", state: "pending", unit_id: "u1", unit_name: "Payments", duration: "4h", proposed_by: "dana@acme.example", proposed_at: NOW, expires_at: NOW, diff: { before: null, after: null, changed: [] }, mine: false, decidable: true, deciders: [{ kind: "security" }], note: { reason: "ship it" } };

beforeEach(() => {
  vi.useFakeTimers();
  pendingFails = false;
  pendingRows = [];
  knows = false;
  waitingFails = false;
  waitingRows = [];
  approveMock.mockReset().mockResolvedValue({});
  declineMock.mockReset().mockResolvedValue({});
  elevationList.mockReset().mockResolvedValue(null);
  elevationApprove.mockReset().mockResolvedValue({});
});
afterEach(() => vi.useRealTimers());

describe("ApprovalsScreen: a decided request is held until a read of its list lands", () => {
  it("a failed read after a decision keeps the card off until a later read lands", async () => {
    pendingRows = [push("p1")];
    await mount("/approvals");
    pendingFails = true;
    fireEvent.click(cardButtons()[0]);
    await flush();
    expect(approveMock).toHaveBeenCalledTimes(1);
    for (const button of cardButtons()) expect(button).toBeDisabled();
    pendingFails = false;
    await flush(10_000);
    for (const button of cardButtons()) expect(button).toBeEnabled();
  });

  it("a card no read frees is freed after the bound", async () => {
    pendingRows = [push("p1")];
    await mount("/approvals");
    pendingFails = true;
    fireEvent.click(cardButtons()[0]);
    await flush();
    await flush(READ_HOLD_MS - 1000);
    for (const button of cardButtons()) expect(button).toBeDisabled();
    await flush(1000);
    for (const button of cardButtons()) expect(button).toBeEnabled();
  });

  it("a Waiting on you read that fails on its own keeps the card off", async () => {
    knows = true;
    waitingRows = [{ ...push("w1"), requested_by: "dana.reyes@acme.example", deciders: ["owner", "unit_admins", "security"] }];
    await mount("/approvals?tab=waiting", false);
    await flush();
    waitingFails = true;
    fireEvent.click(cardButtons()[0]);
    await flush();
    expect(approveMock).toHaveBeenCalledTimes(1);
    for (const button of cardButtons()) expect(button).toBeDisabled();
    waitingFails = false;
    await flush(10_000);
    for (const button of cardButtons()) expect(button).toBeEnabled();
  });

  it("an elevation read a newer one superseded keeps the card off until that newer read lands", async () => {
    const first = later<unknown>();
    const second = later<unknown>();
    elevationList.mockReset().mockResolvedValueOnce(list([e1])).mockReturnValueOnce(first.promise)
      .mockReturnValueOnce(second.promise).mockResolvedValue(list([]));
    const buttons = () => [screen.getByRole("button", { name: /^approve$/i }), screen.getByRole("button", { name: /^deny$/i })];
    await mount("/approvals", false);
    fireEvent.mouseDown(screen.getByRole("tab", { name: /waiting on you/i }));
    await flush();
    fireEvent.click(buttons()[0]);
    await flush();
    await flush(10_000);
    expect(elevationList).toHaveBeenCalledTimes(3);
    await act(async () => first.answer(list([e1])));
    await flush();
    for (const button of buttons()) expect(button).toBeDisabled();
    await act(async () => second.answer(list([])));
    await flush();
    expect(screen.queryByRole("button", { name: /^approve$/i })).toBeNull();
  });

  it("an approval card is freed by the approvals read even when the elevation read fails", async () => {
    pendingRows = [push("p1")];
    await mount("/approvals");
    elevationList.mockReset().mockRejectedValue(new Error("read failed"));
    fireEvent.click(cardButtons()[0]);
    await flush();
    expect(approveMock).toHaveBeenCalledTimes(1);
    for (const button of cardButtons()) expect(button).toBeEnabled();
  });
});

// The server's 409 for a request someone else decided first: the card is stale, so the lists are read again at once.
// The request stays held until a read of its list that began after the 409 has landed, as after a decision that went
// through, so a read that fails does not draw the decided request with live buttons from the list shown before.
describe("ApprovalsScreen: a request refused as already decided is held until a read of its list lands", () => {
  const refused = (sentence = "approval already decided") => new HttpError(409, sentence, "approval_already_decided");

  it("a card's own Approve refused as already decided stays off while the read after it fails, until a later read lands", async () => {
    pendingRows = [push("p1")];
    await mount("/approvals");
    approveMock.mockReset().mockRejectedValueOnce(refused());
    pendingFails = true;
    fireEvent.click(cardButtons()[0]);
    await flush();
    expect(approveMock).toHaveBeenCalledTimes(1);
    for (const button of cardButtons()) expect(button).toBeDisabled();
    pendingFails = false;
    await flush(10_000);
    for (const button of cardButtons()) expect(button).toBeEnabled();
  });

  it("a card refused as already decided that no read frees is freed after the bound", async () => {
    pendingRows = [push("p1")];
    await mount("/approvals");
    approveMock.mockReset().mockRejectedValueOnce(refused());
    pendingFails = true;
    fireEvent.click(cardButtons()[0]);
    await flush();
    await flush(READ_HOLD_MS - 1000);
    for (const button of cardButtons()) expect(button).toBeDisabled();
    await flush(1000);
    for (const button of cardButtons()) expect(button).toBeEnabled();
  });

  it("the reason dialog refused as already decided closes, and its card stays off while the read after it fails", async () => {
    pendingRows = [egress("p1")];
    await mount("/approvals");
    approveMock.mockReset().mockRejectedValueOnce(refused());
    fireEvent.click(screen.getByRole("button", { name: /^approve$/i }));
    await flush();
    pendingFails = true;
    fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: /^approve$/i }));
    await flush();
    expect(approveMock).toHaveBeenCalledTimes(1);
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(screen.getByRole("button", { name: /^approve$/i })).toBeDisabled();
    pendingFails = false;
    await flush(10_000);
    expect(screen.getByRole("button", { name: /^approve$/i })).toBeEnabled();
  });

  it("a Decline refused as no longer open stays off while the read after it fails, until a later read lands", async () => {
    pendingRows = [takeover("t1")];
    await mount("/approvals");
    declineMock.mockReset().mockRejectedValueOnce(refused("this takeover request is no longer open"));
    pendingFails = true;
    const decline = () => within(screen.getByTestId("takeover-card")).getByRole("button", { name: /^Decline$/ });
    fireEvent.click(decline());
    await flush();
    expect(declineMock).toHaveBeenCalledTimes(1);
    expect(decline()).toBeDisabled();
    pendingFails = false;
    await flush(10_000);
    expect(decline()).toBeEnabled();
  });
});
