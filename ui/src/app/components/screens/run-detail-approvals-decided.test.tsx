/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The run's Approvals tab, told by the server that a request was already decided (409 approval_already_decided), reads
// its list at once: the stale card goes, and a reason dialog closes with it. Any other refusal reads nothing.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within, fireEvent } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import type { ApprovalRequest } from "../../lib/types";

const getRunMock = vi.fn();
vi.mock("../../lib/api/runs", () => ({
  runs: {
    getRun: (...a: unknown[]) => getRunMock(...a),
    getGrants: vi.fn().mockResolvedValue([]),
    killRun: vi.fn(),
    getFiles: vi.fn().mockRejectedValue(new Error("no runner in this test")),
    getResources: vi.fn().mockRejectedValue(new Error("no runner in this test")),
    getModelUsage: vi.fn().mockResolvedValue({ totals: { input_tokens: 0, output_tokens: 0, cache_read_tokens: 0, cache_write_tokens: 0, reasoning_tokens: 0, calls: 0, uncounted_calls: 0, partial_calls: 0 }, by_model: [], opaque_tunnels: 0 }),
    getAttachHolder: vi.fn().mockResolvedValue({ held: false }),
    takeoverAttach: vi.fn(),
    getPolicy: vi.fn().mockRejectedValue(new Error("no runner in this test")),
  },
}));
const listApprovalsMock = vi.fn();
const approveMock = vi.fn();
const declineMock = vi.fn();
vi.mock("../../lib/api/approvals", () => ({
  approvals: {
    listApprovals: (...a: unknown[]) => listApprovalsMock(...a),
    approve: (...a: unknown[]) => approveMock(...a),
    deny: vi.fn(),
    decline: (...a: unknown[]) => declineMock(...a),
  },
}));
vi.mock("../../lib/api/audit", async (importOriginal) => ({
  audit: { listAudit: vi.fn().mockResolvedValue([]) },
  egressFromAudit: () => [],
  exitCodeFromAudit: (await importOriginal<typeof import("../../lib/api/audit")>()).exitCodeFromAudit,
  createRequestFromAudit: () => ({}),
  runEndingFromAudit: () => undefined,
}));
vi.mock("../../lib/api/recordings", () => ({ recordings: { getRecording: vi.fn().mockResolvedValue(null) } }));
vi.mock("../../lib/api/health", () => ({ health: { health: vi.fn().mockResolvedValue({}) } }));
vi.mock("sonner", () => ({ toast: { warning: vi.fn(), error: vi.fn(), success: vi.fn() } }));

import { RunDetailScreen } from "./run-detail";
import { OperatorProvider } from "../wardyn/operator-context";
import { ModelAccessProvider } from "../wardyn/model-access-context";
import { HttpError } from "../../lib/api/core";
import { APPROVALS_TAB } from "../wardyn/copy/approvals";

const ME = "member@corp";
const RUN_ROW = {
  id: "run-1",
  created_at: new Date().toISOString(),
  updated_at: new Date().toISOString(),
  created_by: ME,
  agent: "claude-code",
  repo: "acme/widgets",
  task: "fetch a package",
  confinement_class: "CC2",
  state: "RUNNING",
  spiffe_id: "spiffe://wardyn.local/agent-run/run-1",
  runner_target: "docker",
  interactive: false,
};
const base = { run_id: "run-1", state: "PENDING", requested_at: new Date().toISOString() };
const EGRESS = { ...base, id: "egress-1", kind: "egress_domain", requested_scope: { host: "registry.example.test" } } as ApprovalRequest;
const PUSH = {
  ...base,
  id: "push-1",
  kind: "push_content",
  requested_scope: { repo: "github.com/acme/widgets", branch: "refs/heads/feature/x", acts_as: "github_token:11111111-1111-1111-1111-111111111111", paths: ["a.txt"], paths_total: 1, commits: ["deadbeef".repeat(5)], paths_digest: "a".repeat(64), acts_as_kind: "github_app", acts_as_label: "dana@acme.example" },
} as ApprovalRequest;
const TAKEOVER = { ...base, id: "takeover-1", kind: "human_takeover", requested_scope: { reason: "stuck on a prompt" } } as ApprovalRequest;
const EGRESS2 = { ...base, id: "egress-2", kind: "egress_domain", requested_scope: { host: "other.example.test" } } as ApprovalRequest;
const decided = () => new HttpError(409, "approval already decided", "approval_already_decided");

// Opens the run's Approvals tab with `rows` pending on the first read and `after` on the ones after it.
async function openTab(rows: ApprovalRequest | ApprovalRequest[], after: ApprovalRequest[] = []) {
  getRunMock.mockResolvedValue(RUN_ROW);
  listApprovalsMock.mockReset().mockResolvedValueOnce([rows].flat()).mockResolvedValue(after);
  render(
    <MemoryRouter initialEntries={["/runs/run-1"]}>
      <OperatorProvider operator={false} securityOperator={true} principal={ME}>
        <ModelAccessProvider status={null} onRefresh={() => {}}>
          <Routes>
            <Route path="/runs/:id" element={<RunDetailScreen />} />
          </Routes>
        </ModelAccessProvider>
      </OperatorProvider>
    </MemoryRouter>,
  );
  const user = userEvent.setup({ pointerEventsCheck: 0 });
  await user.click(await screen.findByRole("tab", { name: /approvals/i }));
  return await screen.findByRole("region", { name: APPROVALS_TAB.WAITING });
}
// The page polls every few seconds; a read inside this window is the one the refusal asked for.
const readsAfter = (before: number) => waitFor(() => expect(listApprovalsMock.mock.calls.length).toBeGreaterThan(before));

beforeEach(() => {
  vi.clearAllMocks();
});

describe("RunDetailScreen Approvals tab: a request the server says was already decided", () => {
  it("a reason dialog's decision reads the list at once and the dialog closes with the card", async () => {
    approveMock.mockRejectedValueOnce(decided());
    const region = await openTab(EGRESS);
    fireEvent.click(within(region).getByRole("button", { name: /^approve$/i }));
    const dialog = await screen.findByRole("dialog");
    const before = listApprovalsMock.mock.calls.length;
    fireEvent.click(within(dialog).getByRole("button", { name: /^approve$/i }));
    await readsAfter(before);
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(screen.queryByText(/registry\.example\.test/)).toBeNull();
  });

  it("a card's own Approve reads the list at once and the card goes", async () => {
    approveMock.mockRejectedValueOnce(decided());
    const region = await openTab(PUSH);
    const before = listApprovalsMock.mock.calls.length;
    fireEvent.click(within(await within(region).findByTestId("push-content-card")).getByRole("button", { name: /^approve$/i }));
    await readsAfter(before);
    await waitFor(() => expect(screen.queryByTestId("push-content-card")).toBeNull());
  });

  it("a Decline of a request that is no longer open reads the list at once and the card goes", async () => {
    declineMock.mockRejectedValueOnce(decided());
    const region = await openTab(TAKEOVER);
    const before = listApprovalsMock.mock.calls.length;
    fireEvent.click(within(await within(region).findByTestId("takeover-card")).getByRole("button", { name: /^decline$/i }));
    await readsAfter(before);
    await waitFor(() => expect(screen.queryByTestId("takeover-card")).toBeNull());
  });

  it("any other refusal reads nothing", async () => {
    approveMock.mockRejectedValueOnce(new HttpError(409, "refused", "approval_not_pending"));
    const region = await openTab(PUSH);
    const before = listApprovalsMock.mock.calls.length;
    fireEvent.click(within(await within(region).findByTestId("push-content-card")).getByRole("button", { name: /^approve$/i }));
    await waitFor(() => expect(approveMock).toHaveBeenCalledTimes(1));
    await new Promise((r) => setTimeout(r, 300));
    expect(listApprovalsMock.mock.calls.length).toBe(before);
  });

  it("a push card's own Approve stays off until the read after an already-decided answer comes back", async () => {
    approveMock.mockRejectedValueOnce(decided());
    const region = await openTab(PUSH);
    let answer: (rows: ApprovalRequest[]) => void = () => {};
    listApprovalsMock.mockReset().mockReturnValueOnce(new Promise<ApprovalRequest[]>((r) => (answer = r))).mockResolvedValue([]);
    const card = await within(region).findByTestId("push-content-card");
    fireEvent.click(within(card).getByRole("button", { name: /^approve$/i }));
    await waitFor(() => expect(listApprovalsMock).toHaveBeenCalledTimes(1));
    await new Promise((r) => setTimeout(r, 50));
    expect(within(card).getByRole("button", { name: /^approve$/i })).toBeDisabled();
    answer([]);
    await waitFor(() => expect(screen.queryByTestId("push-content-card")).toBeNull());
  });

  it("a Decline stays off until the read after an already-decided answer comes back", async () => {
    declineMock.mockRejectedValueOnce(decided());
    const region = await openTab(TAKEOVER);
    let answer: (rows: ApprovalRequest[]) => void = () => {};
    listApprovalsMock.mockReset().mockReturnValueOnce(new Promise<ApprovalRequest[]>((r) => (answer = r))).mockResolvedValue([]);
    const card = await within(region).findByTestId("takeover-card");
    fireEvent.click(within(card).getByRole("button", { name: /^decline$/i }));
    await waitFor(() => expect(listApprovalsMock).toHaveBeenCalledTimes(1));
    await new Promise((r) => setTimeout(r, 50));
    expect(within(card).getByRole("button", { name: /^decline$/i })).toBeDisabled();
    answer([]);
    await waitFor(() => expect(screen.queryByTestId("takeover-card")).toBeNull());
  });

  it.each([
    ["goes through", (a: { ok: () => void; fail: (e: unknown) => void }) => a.ok()],
    ["is refused as already decided", (a: { ok: () => void; fail: (e: unknown) => void }) => a.fail(decided())],
  ])("a decision that %s after its dialog was closed does not close the dialog opened since for another request", async (_name, settle) => {
    let ok: () => void = () => {};
    let fail: (e: unknown) => void = () => {};
    approveMock.mockReturnValueOnce(new Promise((res, rej) => ((ok = () => res({})), (fail = rej))));
    const region = await openTab([EGRESS, EGRESS2], [EGRESS2]);
    const approves = within(region).getAllByRole("button", { name: /^approve$/i });
    fireEvent.click(approves[0]);
    fireEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: /^approve$/i }));
    await waitFor(() => expect(approveMock).toHaveBeenCalledTimes(1));
    fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: /^cancel$/i }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    fireEvent.click(within(region).getAllByRole("button", { name: /^approve$/i })[1]);
    await screen.findByRole("dialog");
    settle({ ok, fail });
    await new Promise((r) => setTimeout(r, 100));
    expect(screen.getByRole("dialog")).toBeInTheDocument();
  });
});
