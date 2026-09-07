/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// fix: "Run not found" only ever listed archived / deleted / stale-link as
// possible reasons — never "you don't have access" — even though the Copy
// link feature two lines above (copyLink) routinely produces exactly that
// case: a member pastes a coworker's copied run link and hits this state
// because they lack access, not because the run is gone. This pins that the
// description now names that reason too (without disambiguating which case
// applies — preserves the anti-enumeration property).
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";

const getRunMock = vi.fn();
vi.mock("../../lib/api/runs", () => ({
  runs: {
    getRun: (...a: unknown[]) => getRunMock(...a),
    getGrants: vi.fn().mockResolvedValue([]),
    killRun: vi.fn(),
    // The cockpit's evidence widgets poll these. They reject here so the rail
    // renders its degraded states — this suite is about the cockpit's own
    // layout decisions, not the widgets (widgets.test.tsx owns those).
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
// Settable so the exec-labeling test below can flip the run's task mode;
// undefined = a harness run (the default every other test wants).
const auditMocks = vi.hoisted(() => ({
  taskMode: undefined as string | undefined,
  ending: undefined as { kind: string; action: string } | undefined,
}));
vi.mock("../../lib/api/audit", () => ({
  audit: { listAudit: (...a: unknown[]) => listAuditMock(...a) },
  egressFromAudit: () => [],
  exitCodeFromAudit: () => undefined,
  taskModeFromAudit: () => auditMocks.taskMode,
  // The failure block reads the run's ending off the same trail. Mocked to
  // "nothing to explain" by default so every existing case here keeps its
  // exact layout; failure-block.test.tsx exercises the real derivation.
  runEndingFromAudit: () => auditMocks.ending,
}));
afterEach(() => {
  auditMocks.taskMode = undefined;
  auditMocks.ending = undefined;
});
const getRecordingMock = vi.fn().mockResolvedValue(null);
vi.mock("../../lib/api/recordings", () => ({
  // Resolves, rather than a bare vi.fn() returning undefined: a FINISHED run on
  // Overview now fetches its cast on mount (the hero pane replays in place), so
  // a mock that is not thenable takes the whole screen down.
  recordings: { getRecording: (...a: unknown[]) => getRecordingMock(...a) },
}));
vi.mock("../../lib/api/health", () => ({
  health: { health: vi.fn().mockResolvedValue({}) },
}));

import { RunDetailScreen } from "./run-detail";
import { RUN_COCKPIT } from "../wardyn/copy";

beforeEach(() => {
  getRunMock.mockReset();
  listApprovalsMock.mockReset();
  listApprovalsMock.mockResolvedValue([]);
  listAuditMock.mockReset();
  listAuditMock.mockResolvedValue([]);
  getRecordingMock.mockReset();
  getRecordingMock.mockResolvedValue(null);
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

describe("RunDetailScreen — 'Run not found' names lack-of-access as a real reason", () => {
  it("mentions not having access alongside archived/deleted/stale-link", async () => {
    getRunMock.mockResolvedValue(undefined);
    render(
      <MemoryRouter initialEntries={["/runs/run-1"]}>
        <Routes>
          <Route path="/runs/:id" element={<RunDetailScreen />} />
        </Routes>
      </MemoryRouter>,
    );

    expect(await screen.findByText("Run not found")).toBeInTheDocument();
    expect(screen.getByText(/don't have access/i)).toBeInTheDocument();
  });
});

// The cockpit's own state machine — which pane the hero shows. The four
// Terminal situations of design board 2d split across two files: states 1 and 2
// (driving / held by another client) live in attach-terminal.tsx and are pinned
// by attach-terminal.test.tsx, because only the socket knows its attach mode.
// These are the two the PARENT decides, from facts about the run.
describe("RunDetailScreen — the hero pane per run situation", () => {
  it("an autonomous run gets the Output pane, not a terminal it cannot open", async () => {
    renderRun({ ...RUN, interactive: false, state: "RUNNING" });
    // RUN_MODE.autonomous.blurb — there is no PTY to type into.
    expect(await screen.findByText(/Runs unattended/i)).toBeInTheDocument();
    expect(screen.getByText("Output")).toBeInTheDocument();
  });

  // Persona-review product finding #9: a run whose form said "No agent, no
  // model" must not be chipped "the agent drives". task_mode lives only in the
  // run.create audit event, so the pane derives it from the trail it already
  // holds.
  it("labels an exec-mode run honestly — a shell command, no agent harness", async () => {
    auditMocks.taskMode = "exec";
    renderRun({ ...RUN, interactive: false, state: "RUNNING" });
    expect(await screen.findByText("exec — shell command, no agent harness")).toBeInTheDocument();
    expect(screen.queryByText(/the agent drives/i)).not.toBeInTheDocument();
  });

  it("a finished run replays in place rather than leaving the biggest pane dead", async () => {
    renderRun({ ...RUN, state: "COMPLETED", interactive: true });
    // Match the pane's own chip, not the word "Recording" — that also names the
    // fourth TAB, and a query that matched both would pass even if the hero
    // pane were empty, which is the exact thing this test exists to catch.
    const pane = await screen.findByTestId("run-terminal-pane");
    expect(pane).toHaveTextContent(RUN_COCKPIT.finishedReplay);
  });

  // M7(b): the reason a run ended badly was in the audit trail all along; the
  // page said only what STATE it was in. The block belongs in the hero pane —
  // on a run that ended badly the replay is not the news.
  it("a FAILED run explains itself in the hero pane, above the replay", async () => {
    auditMocks.ending = { kind: "image", action: "run.build" };
    renderRun({ ...RUN, state: "FAILED" });
    const block = await screen.findByTestId("run-failure-block");
    expect(screen.getByTestId("run-terminal-pane")).toContainElement(block);
    expect(block).toHaveTextContent("What happened");
  });

  it("keeps run.task as the page's h1 — for an autonomous run it is the only statement of what it is doing", async () => {
    renderRun({ ...RUN });
    expect(
      await screen.findByRole("heading", { name: "audit the egress proxy", level: 1 }),
    ).toBeInTheDocument();
  });
});

// THE POINT OF THE REDESIGN, and therefore the assertion most worth pinning: a
// held egress request renders in the TERMINAL'S OWN COLUMN, under the output
// that caused it — not in a sidebar, not a toast. Asserting it exists somewhere
// on the page would pass for a layout that put it back in the rail, so this
// asserts CONTAINMENT.
describe("RunDetailScreen — the held approval renders inside the terminal pane", () => {
  it("puts the live-approvals strip inside the terminal pane, not the evidence rail", async () => {
    listApprovalsMock.mockResolvedValue([
      {
        id: "a1",
        run_id: "run-1",
        kind: "egress_domain",
        state: "PENDING",
        requested_at: new Date().toISOString(),
        requested_scope: { host: "api.github.com", mode: "wait_for_review" },
      },
    ]);
    renderRun({ ...RUN, state: "RUNNING" });

    const strip = await screen.findByTestId("live-approvals");
    const pane = screen.getByTestId("run-terminal-pane");
    expect(pane).toContainElement(strip);
  });

  it("states the hold in the command bar too, so it is visible without scrolling", async () => {
    listApprovalsMock.mockResolvedValue([
      {
        id: "a1",
        run_id: "run-1",
        kind: "egress_domain",
        state: "PENDING",
        requested_at: new Date().toISOString(),
        requested_scope: { host: "api.github.com", mode: "wait_for_review" },
      },
    ]);
    renderRun({ ...RUN, state: "RUNNING" });
    // waitingHeld, not waiting: a parked sandbox is a different fact from a
    // merely queued approval, and isHeld is shared with the strip below so the
    // two can never disagree.
    expect(await screen.findByText(/sandbox held/i)).toBeInTheDocument();
  });
});

// hasWorkspace must cover workspace_id, not just workspace_ids: a record/
// verify step run carries the former only (its workspace_ids is always
// empty — see runHasWorkspace's doc in lib/types/runs.ts), and the server's
// rule-5 tie-break accepts `always` there. Reading workspace_ids alone showed
// Always disabled with the false reason "this run isn't attached to one".
describe("RunDetailScreen — hasWorkspace covers workspace_id, not just workspace_ids", () => {
  it("a record/verify-shaped run (workspace_id set, workspace_ids empty) offers Always", async () => {
    listApprovalsMock.mockResolvedValue([
      {
        id: "a1",
        run_id: "run-1",
        kind: "egress_domain",
        state: "PENDING",
        requested_at: new Date().toISOString(),
        requested_scope: { host: "api.github.com" },
      },
    ]);
    renderRun({ ...RUN, state: "RUNNING", workspace_ids: [], workspace_id: "ws-1" });

    const strip = await screen.findByTestId("live-approvals");
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    await user.click(within(strip).getAllByRole("button", { name: /more options/i })[0]);

    const always = await screen.findByRole("button", { name: /^Always/ });
    expect(always).not.toBeDisabled();
    expect(screen.queryByText(/isn't attached to one/i)).not.toBeInTheDocument();
  });

  it("an ordinary run with neither field set still shows Always disabled, with why", async () => {
    listApprovalsMock.mockResolvedValue([
      {
        id: "a1",
        run_id: "run-1",
        kind: "egress_domain",
        state: "PENDING",
        requested_at: new Date().toISOString(),
        requested_scope: { host: "api.github.com" },
      },
    ]);
    renderRun({ ...RUN, state: "RUNNING" });

    const strip = await screen.findByTestId("live-approvals");
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    await user.click(within(strip).getAllByRole("button", { name: /more options/i })[0]);

    const always = await screen.findByRole("button", { name: /^Always/ });
    expect(always).toBeDisabled();
    expect(screen.getByText(/isn't attached to one/i)).toBeInTheDocument();
  });
});

// D9: a pre-agent-start failure (mount failure, etc.) never gets an exit code
// at all — failure_hint is the only place that run says why. Bare server
// text, no prefix (the state badge already says "Failed").
describe("RunDetailScreen — D9 failure-hint chip", () => {
  it("a FAILED run with failure_hint shows the bare server text", async () => {
    renderRun({ ...RUN, state: "FAILED", failure_hint: "image not found on daemon" });
    expect(await screen.findByText("image not found on daemon")).toBeInTheDocument();
  });

  it("a FAILED run without failure_hint shows no hint chip", async () => {
    renderRun({ ...RUN, state: "FAILED" });
    await screen.findByText("Failed");
    expect(screen.queryByText("image not found on daemon")).not.toBeInTheDocument();
  });
});

// W17-S1-3: the run-detail Audit tab's own fetch is capped at LIST_LIMIT
// (server: auditPerRunDefaultLimit) and returned oldest-first — a chatty run's
// later events can silently fall off the end. A capped page must say so; a
// page under the cap must not.
// 15s suite default, not vitest's 5s: the capped case renders a 1000-row audit
// list and then drives a userEvent click through it — under a second alone, but
// deterministically over the 5s ceiling when all 79 files run in parallel.
// Same rationale (and the same 1000-row feed) as audit.test.tsx's suite timeout.
describe("RunDetailScreen — Audit tab truncation cue", { timeout: 15_000 }, () => {
  function auditEvent(i: number) {
    return {
      id: `e${i}`,
      time: new Date().toISOString(),
      actor_type: "agent",
      actor: "spiffe://wardyn/agent",
      action: "kernel.process.exec",
      outcome: "success",
      run_id: "run-1",
    };
  }

  it("shows a truncation cue when the per-run window hits the 1000 cap", async () => {
    listAuditMock.mockImplementation((_id: string, action?: string) =>
      Promise.resolve(action ? [] : Array.from({ length: 1000 }, (_, i) => auditEvent(i))),
    );
    renderRun(RUN);
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    await user.click(await screen.findByRole("tab", { name: /audit/i }));

    expect(await screen.findByText(/truncated, oldest-first/i)).toBeInTheDocument();
  });

  it("shows no truncation cue under the cap", async () => {
    listAuditMock.mockImplementation((_id: string, action?: string) =>
      Promise.resolve(action ? [] : [auditEvent(1)]),
    );
    renderRun(RUN);
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    await user.click(await screen.findByRole("tab", { name: /audit/i }));

    await screen.findByText(/1 event for this run/i);
    expect(screen.queryByText(/truncated, oldest-first/i)).not.toBeInTheDocument();
  });
});

// M5: a tool call the run's own tool_rules answered creates no approval card, so
// the audit trail is the only place that decision is visible — and it rides an
// egress row whose target is the control plane. The tab must say who decided.
describe("RunDetailScreen — Audit tab names a rule-decided tool call", () => {
  it("renders the decision and the rule, not the control-plane host", async () => {
    listAuditMock.mockImplementation((_id: string, action?: string) =>
      Promise.resolve(
        action
          ? []
          : [
              {
                id: "e1",
                time: new Date().toISOString(),
                actor_type: "system",
                actor: "proxy",
                action: "egress.allow",
                outcome: "allow",
                run_id: "run-1",
                target: "wardynd:8080",
                data: { rule_source: "policy:tool-allow" },
              },
            ],
      ),
    );
    renderRun(RUN);
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    await user.click(await screen.findByRole("tab", { name: /audit/i }));

    expect(await screen.findByText("Decided by rule")).toBeInTheDocument();
    expect(screen.getByText("policy:tool-allow")).toBeInTheDocument();
    expect(screen.queryByText("wardynd:8080")).not.toBeInTheDocument();
  });
});

// W25-W25.2-3: /audit is run-scoped for a member (empty 200 without ?run_id=),
// so the Audit tab's "open full Audit" link must carry the run — a bare /audit
// drops a member on a feed that can never fill.
describe("RunDetailScreen — open full Audit link", () => {
  it("carries the run id into /audit", async () => {
    renderRun(RUN);
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    await user.click(await screen.findByRole("tab", { name: /audit/i }));

    expect(await screen.findByRole("link", { name: /open full audit/i })).toHaveAttribute(
      "href",
      "/audit?run_id=run-1",
    );
  });
});

// R4-F002: the Approvals tab used to pull the WHOLE fleet's list
// (listApprovals("")) and filter in the browser — i.e. AFTER the server's
// requested_at DESC window — so past LIST_LIMIT lifetime approvals a run's own
// holds vanished from its own detail page, PENDING badge and all
// (internal/api/approvals.go:56-61). The fetch must carry ?run_id=.
describe("RunDetailScreen — its approvals are scoped server-side", () => {
  it("asks /approvals for THIS run, so an old run past the list window still shows its holds", async () => {
    // The honest server: an un-scoped read answers with the newest window,
    // which for an old run contains none of its rows.
    listApprovalsMock.mockImplementation((_state: unknown, runId: unknown) =>
      Promise.resolve(
        runId === "run-1"
          ? [
              {
                id: "ap-old",
                run_id: "run-1",
                kind: "egress_domain",
                requested_scope: { host: "unlisted.example" },
                state: "PENDING",
                requested_at: new Date().toISOString(),
              },
            ]
          : [],
      ),
    );
    renderRun(RUN);

    const tab = await screen.findByRole("tab", { name: /approvals/i });
    expect(tab).toHaveTextContent("1");
    expect(listApprovalsMock).toHaveBeenCalledWith("", "run-1");
  });
});

// R4-F004: the hero pane's notice had three arms — loading/idle,
// recordingDisabled, else recordingMissing — so a FAILED getRecording fell
// through to "This run has no captured terminal session", a statement about the
// RUN made from a fetch that never established it. The Recording tab, fed by
// the same recState, said "Couldn't load this run's recording." — one failed
// fetch, two contradictory answers, and the one on the DEFAULT tab was false.
describe("RunDetailScreen — a failed recording fetch is not a claim about the run", () => {
  it("says the recording could not be loaded, never that the run has none", async () => {
    getRecordingMock.mockRejectedValue(new Error("recording store unreachable"));
    renderRun({ ...RUN, state: "COMPLETED", interactive: true });

    const pane = await screen.findByTestId("run-terminal-pane");
    await waitFor(() => expect(pane).toHaveTextContent(RUN_COCKPIT.recordingError));
    expect(pane).not.toHaveTextContent(RUN_COCKPIT.recordingMissing);
  });

  it("still says the run has none when the fetch SUCCEEDS with no cast", async () => {
    getRecordingMock.mockResolvedValue(null);
    renderRun({ ...RUN, state: "COMPLETED", interactive: true });

    const pane = await screen.findByTestId("run-terminal-pane");
    await waitFor(() => expect(pane).toHaveTextContent(RUN_COCKPIT.recordingMissing));
  });
});

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
const CONTROL_PLANE_SENTENCE = "We couldn't reach the Wardyn control plane. Please try again.";

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
