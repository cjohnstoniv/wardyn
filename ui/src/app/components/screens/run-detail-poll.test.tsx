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
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Link, MemoryRouter, Route, Routes, useLocation } from "react-router-dom";

const getRunMock = vi.fn();
const killRunMock = vi.fn().mockResolvedValue(undefined);
vi.mock("../../lib/api/runs", () => ({
  runs: {
    getRun: (...a: unknown[]) => getRunMock(...a),
    getGrants: vi.fn().mockResolvedValue([]),
    killRun: (...a: unknown[]) => killRunMock(...a),
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

// #1483: the route changed but the screen did not remount, so run A's late
// answers (and its poll, its approvals filter, its detached ending fetch)
// landed on run B's page, and the Kill dialog named A while sending B.
describe("RunDetailScreen — a route change drops everything run A was doing", () => {
  const A = { ...RUN, id: "run-a", task: "Distinct task A" };
  const B = { ...RUN, id: "run-b", task: "Distinct task B" };

  function Location() {
    return <output data-testid="pathname">{useLocation().pathname}</output>;
  }

  function renderAB(prefix: "/runs" | "/admin/runs" = "/runs") {
    return render(
      <MemoryRouter initialEntries={[`${prefix}/run-a`]}>
        <Location />
        <Link to={`${prefix}/run-b`}>Switch to B</Link>
        <Routes>
          <Route path="/runs/:id" element={<RunDetailScreen />} />
          <Route path="/admin/runs/:id" element={<RunDetailScreen />} />
        </Routes>
      </MemoryRouter>,
    );
  }

  function seed(onA: () => Promise<unknown>) {
    getRunMock.mockImplementation((id: string) => (id === "run-a" ? onA() : Promise.resolve(B)));
  }

  for (const prefix of ["/runs", "/admin/runs"] as const) {
    it(`${prefix}: A answering late never renders on B, and the Kill dialog names the run it kills`, async () => {
      const user = userEvent.setup();
      let settleA!: (v: unknown) => void;
      seed(() => new Promise((r) => (settleA = r)));
      renderAB(prefix);
      await waitFor(() => expect(getRunMock).toHaveBeenCalledWith("run-a"));
      await user.click(screen.getByRole("link", { name: "Switch to B" }));
      await screen.findAllByText("Distinct task B");
      await act(async () => settleA(A));
      expect(screen.getByTestId("pathname")).toHaveTextContent(`${prefix}/run-b`);
      expect(screen.queryAllByText("Distinct task A")).toHaveLength(0);
      expect(screen.queryAllByText("Distinct task B").length).toBeGreaterThan(0);

      await user.click(screen.getByRole("button", { name: /^Kill/ }));
      const dialog = screen.getByRole("alertdialog");
      expect(dialog).toHaveAccessibleName("Kill run-b?");
      await user.click(within(dialog).getByRole("button", { name: "Kill run" }));
      expect(killRunMock).toHaveBeenCalledTimes(1);
      expect(killRunMock).toHaveBeenCalledWith("run-b");
    });
  }

  it("a late REJECTION of A does not error B's page", async () => {
    const user = userEvent.setup();
    let rejectA!: (e: unknown) => void;
    seed(() => new Promise((_, rej) => (rejectA = rej)));
    renderAB();
    await waitFor(() => expect(getRunMock).toHaveBeenCalledWith("run-a"));
    await user.click(screen.getByRole("link", { name: "Switch to B" }));
    await screen.findAllByText("Distinct task B");
    await act(async () => rejectA(new Error("A went away")));
    expect(screen.queryByText(CONTROL_PLANE_SENTENCE)).toBeNull();
    expect(screen.queryAllByText("Distinct task B").length).toBeGreaterThan(0);
  });

  it("B's Approvals strip shows nothing from A's late approvals answer", async () => {
    const user = userEvent.setup();
    let settleApprovals!: (v: unknown) => void;
    getRunMock.mockImplementation((id: string) => Promise.resolve(id === "run-a" ? A : B));
    listApprovalsMock.mockImplementation((_s: string, id: string) =>
      id === "run-a" ? new Promise((r) => (settleApprovals = r)) : Promise.resolve([]),
    );
    renderAB();
    await waitFor(() => expect(listApprovalsMock).toHaveBeenCalledWith("", "run-a"));
    await user.click(screen.getByRole("link", { name: "Switch to B" }));
    await screen.findAllByText("Distinct task B");
    await act(async () =>
      settleApprovals([{ id: "ap-1", run_id: "run-a", kind: "egress_domain", state: "PENDING", scope: { host: "a.example" } }]),
    );
    // A pending approval on the tab label would read "Approvals 1".
    expect(screen.getByRole("tab", { name: /Approvals/ })).toHaveTextContent(/^Approvals$/);
  });

  describe("polling", () => {
    beforeEach(() => vi.useFakeTimers({ shouldAdvanceTime: true }));
    afterEach(() => vi.useRealTimers());

    it("B polls on schedule even though A's tick never answered", async () => {
      let ticksOfA = 0;
      getRunMock.mockImplementation((id: string) => {
        if (id === "run-b") return Promise.resolve(B);
        ticksOfA += 1;
        // A's foreground load answers; every later tick hangs.
        return ticksOfA === 1 ? Promise.resolve(A) : new Promise(() => {});
      });
      renderAB();
      await screen.findAllByText("Distinct task A");
      await vi.advanceTimersByTimeAsync(4000); // A's poll starts and stalls
      await act(async () => screen.getByRole("link", { name: "Switch to B" }).click());
      await screen.findAllByText("Distinct task B");
      const bCalls = () => getRunMock.mock.calls.filter((c) => c[0] === "run-b").length;
      const before = bCalls();
      await vi.advanceTimersByTimeAsync(4000);
      expect(bCalls()).toBeGreaterThan(before);
    });
  });
});

// Review nit: the page drops an answer for a different run id; a non-canonical
// (upper-case) UUID in the URL must not read as "a different run" and spin.
describe("RunDetailScreen — a run id in the URL is matched case-insensitively", () => {
  it("renders the run when the URL's id differs from the answer's only in case", async () => {
    getRunMock.mockResolvedValue({ ...RUN, id: "5f0c1a2b-aaaa-bbbb-cccc-0123456789ab" });
    render(
      <MemoryRouter initialEntries={["/runs/5F0C1A2B-AAAA-BBBB-CCCC-0123456789AB"]}>
        <Routes>
          <Route path="/runs/:id" element={<RunDetailScreen />} />
        </Routes>
      </MemoryRouter>,
    );
    expect((await screen.findAllByText(RUN.task)).length).toBeGreaterThan(0);
  });
});

// Review G4: the approvals filter matches run ids case-insensitively too.
describe("RunDetailScreen — approvals match the run id case-insensitively", () => {
  it("counts a pending approval whose run_id differs from the URL's only in case", async () => {
    listApprovalsMock.mockResolvedValue([
      { id: "ap-1", run_id: "RUN-1", kind: "egress_domain", state: "PENDING", scope: { host: "a.example" } },
    ]);
    renderRun({ ...RUN, state: "RUNNING" });
    await screen.findAllByText(RUN.task);
    expect(await screen.findByRole("tab", { name: /Approvals\s*1/ })).toBeInTheDocument();
  });
});
