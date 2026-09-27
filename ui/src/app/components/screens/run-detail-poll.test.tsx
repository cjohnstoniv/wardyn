/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The run cockpit against a slow, failing or unwatched control plane: how
// load() and its poll behave when GET /runs/{id} or a side fetch hangs or
// rejects. Split out of run-detail.test.tsx (over the file-size cap) along
// that seam; it needs only the api fakes below, none of that file's recording
// or audit-row fixtures.

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

const getRunMock = vi.fn();
vi.mock("../../lib/api/runs", () => ({
  runs: {
    getRun: (...a: unknown[]) => getRunMock(...a),
    getGrants: vi.fn().mockResolvedValue([]),
    killRun: vi.fn(),
    getFiles: vi.fn().mockRejectedValue(new Error("no runner in this test")),
    getResources: vi.fn().mockRejectedValue(new Error("no runner in this test")),
    getAttachHolder: vi.fn().mockResolvedValue({ held: false }),
    takeoverAttach: vi.fn(),
  },
}));
const listApprovalsMock = vi.fn().mockResolvedValue([]);
vi.mock("../../lib/api/approvals", () => ({
  approvals: {
    listApprovals: (...a: unknown[]) => listApprovalsMock(...a),
    approve: vi.fn(),
    deny: vi.fn(),
  },
}));
const listAuditMock = vi.fn().mockResolvedValue([]);
vi.mock("../../lib/api/audit", () => ({
  audit: { listAudit: (...a: unknown[]) => listAuditMock(...a) },
  egressFromAudit: () => [],
  exitCodeFromAudit: () => undefined,
  createRequestFromAudit: () => ({}),
  runEndingFromAudit: () => undefined,
}));
vi.mock("../../lib/api/recordings", () => ({
  recordings: { getRecording: vi.fn().mockResolvedValue(null) },
}));
vi.mock("../../lib/api/health", () => ({
  health: { health: vi.fn().mockResolvedValue({}) },
}));
vi.mock("sonner", () => ({ toast: { warning: vi.fn(), error: vi.fn(), success: vi.fn() } }));

import { RunDetailScreen } from "./run-detail";
import { STATES } from "../wardyn/states";

beforeEach(() => {
  vi.clearAllMocks();
  getRunMock.mockReset();
  listApprovalsMock.mockReset();
  listApprovalsMock.mockResolvedValue([]);
  listAuditMock.mockReset();
  listAuditMock.mockResolvedValue([]);
});

const RUN = {
  id: "run-1",
  created_at: new Date().toISOString(),
  updated_at: new Date().toISOString(),
  created_by: "me",
  agent: "claude-code",
  repo: "acme/widgets",
  task: "audit the egress proxy",
  confinement_class: "CC2",
  state: "RUNNING",
  spiffe_id: "spiffe://wardyn.local/agent-run/run-1",
  runner_target: "docker",
  interactive: false,
};

function renderRun(run: Record<string, unknown>) {
  getRunMock.mockResolvedValue(run);
  return render(
    <MemoryRouter initialEntries={["/runs/run-1"]}>
      <Routes>
        <Route path="/runs/:id" element={<RunDetailScreen />} />
      </Routes>
    </MemoryRouter>,
  );
}

// R4-F073/F074: the cockpit fires FIVE requests per DETAIL_POLL_MS tick, and
// load() used to fire-and-forget — so a control plane slower than the interval
// stacked a new set of five on every tick (34 concurrent in-flight measured at
// 12s latency, 97 at 40s), and a BACKGROUNDED tab kept paying all of it for a
// human who could see none of it. load() now RETURNS its Promise.all so
// usePoll's in-flight guard has something to wait on.
describe("RunDetailScreen — a slow or unwatched control plane costs one set of requests, not one per tick", () => {
  beforeEach(() => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
  });
  afterEach(() => {
    vi.useRealTimers();
    // Only the document.hidden spy is undone here: vi.restoreAllMocks() would
    // also strip the module mocks' inline mockResolvedValue()s, which this
    // file's beforeEach does not re-seed.
    hiddenSpy?.mockRestore();
    hiddenSpy = null;
  });
  let hiddenSpy: ReturnType<typeof vi.spyOn> | null = null;

  it("does not stack a second poll on top of one that has not answered", async () => {
    let settleRun: ((r: unknown) => void) | null = null;
    getRunMock.mockReset();
    // The FIRST (foreground) load answers so the screen renders; every poll
    // after it hangs, which is exactly the slow-backend case.
    getRunMock
      .mockResolvedValueOnce({ ...RUN, state: "RUNNING" })
      .mockImplementation(() => new Promise((res) => (settleRun = res)));

    render(
      <MemoryRouter initialEntries={["/runs/run-1"]}>
        <Routes>
          <Route path="/runs/:id" element={<RunDetailScreen />} />
        </Routes>
      </MemoryRouter>,
    );
    await waitFor(() => expect(getRunMock).toHaveBeenCalled());
    await screen.findAllByText(RUN.task);
    const afterFirstLoad = getRunMock.mock.calls.length;

    // One tick starts a poll; five more intervals go by with it unanswered.
    await vi.advanceTimersByTimeAsync(4000);
    expect(getRunMock.mock.calls.length).toBe(afterFirstLoad + 1);
    await vi.advanceTimersByTimeAsync(20_000);
    expect(getRunMock.mock.calls.length).toBe(afterFirstLoad + 1);

    // ...and the poller is not wedged: the moment it answers, ticks resume.
    settleRun!({ ...RUN, state: "RUNNING" });
    await vi.advanceTimersByTimeAsync(4000);
    expect(getRunMock.mock.calls.length).toBeGreaterThan(afterFirstLoad + 1);
  });

  it("polls nothing at all while the tab is hidden", async () => {
    getRunMock.mockReset();
    getRunMock.mockResolvedValue({ ...RUN, state: "RUNNING" });
    render(
      <MemoryRouter initialEntries={["/runs/run-1"]}>
        <Routes>
          <Route path="/runs/:id" element={<RunDetailScreen />} />
        </Routes>
      </MemoryRouter>,
    );
    await screen.findAllByText(RUN.task);

    hiddenSpy = vi.spyOn(document, "hidden", "get").mockReturnValue(true) as never;
    document.dispatchEvent(new Event("visibilitychange"));
    const before = getRunMock.mock.calls.length;
    await vi.advanceTimersByTimeAsync(60_000); // fifteen ticks nobody can see
    expect(getRunMock.mock.calls.length).toBe(before);
  });
});

// R4-F141: load() awaited FIVE fetches with Promise.all, so ONE subsidiary
// rejection took the whole cockpit down to ErrorState's "We couldn't reach the
// Wardyn control plane" — an outage claim that is false when GET /runs/{id}
// answered 200, and one that costs a live run its Kill button, its terminal and
// its approvals strip. The rejection is routine: handleListApprovals answers
// 500 "approval listing is not scoped for members on this backend" without
// ApprovalsByRunCreatorPager (internal/api/approvals.go), and a degraded audit
// store fails listAudit.
const CONTROL_PLANE_SENTENCE = STATES.ERROR_DEFAULT;

describe("RunDetailScreen — a failing side fetch is not an outage", () => {
  it("keeps the cockpit — heading, state and Kill — when audit and approvals both fail", async () => {
    listAuditMock.mockReset();
    listAuditMock.mockRejectedValue(new Error("audit store degraded"));
    listApprovalsMock.mockReset();
    listApprovalsMock.mockRejectedValue(new Error("approval listing is not scoped for members"));

    renderRun({ ...RUN, state: "RUNNING" });

    // The run rendered, so the page must say what the run says.
    expect((await screen.findAllByText(RUN.task)).length).toBeGreaterThan(0);
    // ...and the one control that ends a runaway run is still reachable.
    expect(screen.getByRole("button", { name: /Kill/ })).toBeInTheDocument();
    expect(screen.queryByText(CONTROL_PLANE_SENTENCE)).toBeNull();
  });

  it("still errors when the RUN itself is the fetch that failed", async () => {
    getRunMock.mockReset();
    getRunMock.mockRejectedValue(new Error("control plane down"));
    render(
      <MemoryRouter initialEntries={["/runs/run-1"]}>
        <Routes>
          <Route path="/runs/:id" element={<RunDetailScreen />} />
        </Routes>
      </MemoryRouter>,
    );
    expect(await screen.findByText(CONTROL_PLANE_SENTENCE)).toBeInTheDocument();
  });
});

// R4-F127: the foreground/background split at the end of load() is the rule
// that a poll blip must not wipe a live cockpit — and nothing tested it, so
// deleting the `if (foreground)` guard left all sixteen tests green. A blip
// here is not cosmetic: the ErrorState it would render carries no run state, no
// terminal and no Kill button, and it would return every DETAIL_POLL_MS.
describe("RunDetailScreen — a background poll blip keeps the last-good cockpit", () => {
  beforeEach(() => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  it("does not replace the page with the control-plane error when a tick fails", async () => {
    getRunMock.mockReset();
    // The foreground load answers; every poll after it rejects.
    getRunMock
      .mockResolvedValueOnce({ ...RUN, state: "RUNNING" })
      .mockRejectedValue(new Error("control plane hiccup"));

    render(
      <MemoryRouter initialEntries={["/runs/run-1"]}>
        <Routes>
          <Route path="/runs/:id" element={<RunDetailScreen />} />
        </Routes>
      </MemoryRouter>,
    );
    await screen.findAllByText(RUN.task);
    const afterFirstLoad = getRunMock.mock.calls.length;

    await vi.advanceTimersByTimeAsync(20_000); // five ticks, all rejecting
    expect(getRunMock.mock.calls.length).toBeGreaterThan(afterFirstLoad);

    expect(screen.queryByText(CONTROL_PLANE_SENTENCE)).toBeNull();
    expect((await screen.findAllByText(RUN.task)).length).toBeGreaterThan(0);
    expect(screen.getByRole("button", { name: /Kill/ })).toBeInTheDocument();
  });
});
