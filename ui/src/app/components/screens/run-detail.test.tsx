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
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import type { AuditEvent } from "../../lib/types";

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
vi.mock("../../lib/api/audit", async (importOriginal) => ({
  audit: { listAudit: (...a: unknown[]) => listAuditMock(...a) },
  egressFromAudit: () => [],
  // F6-F2: the REAL derivation, not a stub — every other test here leaves
  // listAuditMock at its default `[]`, which the real function already reads
  // as "no exit code" (identical to the old stub); only F6-F2's own test
  // seeds a run.complete row and needs the real scan to see it.
  exitCodeFromAudit: (await importOriginal<typeof import("../../lib/api/audit")>()).exitCodeFromAudit,
  // B4b: the request-scoped half of the run, off its run.create row —
  // task_mode included (it replaced taskModeFromAudit here), plus the fields a
  // clone needs that the run record never held.
  createRequestFromAudit: () => ({ task_mode: auditMocks.taskMode }),
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
// review U-01: the header's clone door (onClone) must toast, not navigate,
// when cloneFromAudit refuses — asserted against the real sonner mock below,
// not a stub that swallows the call silently.
vi.mock("sonner", () => ({ toast: { warning: vi.fn(), error: vi.fn(), success: vi.fn() } }));

import { RunDetailScreen } from "./run-detail";
import { RUN_COCKPIT, SECURITY_ONLY_REASON } from "../wardyn/copy";
import { OperatorProvider } from "../wardyn/operator-context";
import { sessionOptionLabel } from "./run-detail/recording-tab-copy";
import { toast } from "sonner";
import { aheadByHours } from "../../lib/test-clock";

beforeEach(() => {
  // review R-07: clears toast.warning/error/success's call history too — the
  // U-01 test asserts `toHaveBeenCalledWith`, not `…Times(1)`, so without
  // this an earlier test toasting the same string would let it pass
  // vacuously.
  vi.clearAllMocks();
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

// F1-F1: `attachable` used to fold "can this caller ever attach?" and "has
// the run reached RUNNING yet?" into one gate, so an interactive run still
// STARTING told its own OWNER the operator-only refusal plus a dead "Watch
// the captured session →" link to a recording that cannot exist yet.
describe("RunDetailScreen — a not-yet-running interactive run tells its owner it's starting, not that they lack the role", () => {
  it("an operator on a STARTING interactive run sees the starting notice, never the admin-role refusal", async () => {
    renderRun({ ...RUN, state: "STARTING", interactive: true });
    expect(await screen.findByText(RUN_COCKPIT.starting)).toBeInTheDocument();
    expect(screen.queryByText("Requires the admin role.")).not.toBeInTheDocument();
    expect(screen.queryByText(/Watch the captured session/)).not.toBeInTheDocument();
  });

  // Neg: a member who does NOT own this run and isn't an operator still gets
  // the real refusal — canAttach must stay false for them regardless of state.
  it("a member who neither owns nor operates a RUNNING run still gets the admin-role refusal", async () => {
    getRunMock.mockResolvedValue({ ...RUN, state: "RUNNING", interactive: true, created_by: "someone-else" });
    render(
      <MemoryRouter initialEntries={["/runs/run-1"]}>
        <OperatorProvider operator={false} principal="me">
          <Routes>
            <Route path="/runs/:id" element={<RunDetailScreen />} />
          </Routes>
        </OperatorProvider>
      </MemoryRouter>,
    );
    expect(await screen.findByText("Requires the admin role.")).toBeInTheDocument();
    expect(screen.queryByText(RUN_COCKPIT.starting)).not.toBeInTheDocument();
  });
});

// F1-F2: the recording fetch had no ordering guard — a slow fetch for an
// earlier-selected session could resolve AFTER a later one and overwrite it.
describe("RunDetailScreen — a stale recording fetch never overwrites a later selection", () => {
  it("keeps the SECOND selection's cast when the first session's fetch resolves later", async () => {
    let resolveFirst!: (v: unknown) => void;
    // A's fetch stays pending; B's resolves to "missing" (null) immediately —
    // if A's stale response later wins, its (truthy) Recording renders the
    // player instead of B's session-scoped empty state.
    getRecordingMock
      .mockImplementationOnce(() => new Promise((res) => { resolveFirst = res; }))
      .mockResolvedValueOnce(null);
    listAuditMock.mockResolvedValue([
      {
        id: "e1",
        time: new Date().toISOString(),
        actor_type: "human",
        actor: "alice",
        action: "session.recording",
        target: "run-1~session-a",
        outcome: "success",
      },
      {
        id: "e2",
        time: new Date().toISOString(),
        actor_type: "human",
        actor: "bob",
        action: "session.recording",
        target: "run-1~session-b",
        outcome: "success",
      },
    ]);
    renderRun({ ...RUN, state: "COMPLETED", interactive: true });
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    await user.click(await screen.findByRole("tab", { name: /recording/i }));

    // Select session A (first fetch starts, stays pending) then session B
    // (second fetch resolves immediately).
    await user.click(screen.getByRole("combobox", { name: "Recorded session" }));
    await user.click(await screen.findByText(/alice/));
    await user.click(screen.getByRole("combobox", { name: "Recorded session" }));
    await user.click(await screen.findByText(/bob/));

    // B's (missing) result should already be showing.
    expect(await screen.findByText("No recording for this session")).toBeInTheDocument();

    // The first (stale) fetch resolves now, with a REAL cast — it must not
    // clobber B's already-settled "missing" state.
    resolveFirst({
      run_id: "run-1",
      header: { version: 2, width: 80, height: 24 },
      events: [],
      cast: "",
    });
    await new Promise((r) => setTimeout(r, 0));
    expect(screen.getByText("No recording for this session")).toBeInTheDocument();
    expect(getRecordingMock).toHaveBeenLastCalledWith("run-1", "run-1~session-b");
  });
});

// F1-F11/F1-F12: the session picker's own copy.
describe("RunDetailScreen — the recording tab's session-picker copy", () => {
  const session = {
    id: "e1",
    time: aheadByHours(-1),
    actor_type: "human",
    actor: "alice",
    action: "session.recording",
    target: "run-1~session-a",
    outcome: "success",
  };

  it("names the session's END, not its start — recordings are emitted at detach", async () => {
    // ticket: F1-F12
    listAuditMock.mockResolvedValue([session]);
    renderRun({ ...RUN, state: "COMPLETED", interactive: true });
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    await user.click(await screen.findByRole("tab", { name: /recording/i }));
    await user.click(screen.getByRole("combobox", { name: "Recorded session" }));

    // R-8: through the constant, not a raw regex re-deriving its shape.
    expect(await screen.findByText(sessionOptionLabel(session as AuditEvent))).toBeInTheDocument();
    expect(screen.queryByText(/^Attached/)).not.toBeInTheDocument();
  });

  it("a picked SESSION with a missing cast gets session-scoped copy, not the run-scoped sentence", async () => {
    // ticket: F1-F11
    listAuditMock.mockResolvedValue([session]);
    getRecordingMock.mockResolvedValue(null);
    renderRun({ ...RUN, state: "COMPLETED", interactive: true });
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    await user.click(await screen.findByRole("tab", { name: /recording/i }));
    await user.click(screen.getByRole("combobox", { name: "Recorded session" }));
    await user.click(await screen.findByText(/alice/));

    expect(await screen.findByText("No recording for this session")).toBeInTheDocument();
    expect(screen.queryByText("No recording available")).not.toBeInTheDocument();
  });

  // Neg: the bare run id (no session picked) keeps the run-scoped sentence.
  it("negative control: the bare run id still gets the run-scoped sentence", async () => {
    // ticket: F1-F11
    getRecordingMock.mockResolvedValue(null);
    renderRun({ ...RUN, state: "COMPLETED", interactive: true });
    await screen.findByTestId("run-terminal-pane");
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    await user.click(screen.getByRole("tab", { name: /recording/i }));

    expect(await screen.findByText("No recording available")).toBeInTheDocument();
    expect(screen.queryByText("No recording for this session")).not.toBeInTheDocument();
  });
});

// F6-F2: run.complete/run.kill/run.autostop are the LATEST events on a run's
// trail — past the 1000-row cap on the general fetch, the exit code and
// ending both silently went "unknown". Scoped fetches keep them known.
describe("RunDetailScreen — the exit code survives a truncated audit trail", () => {
  it("still knows the exit code past 1000 rows of unrelated audit history", async () => {
    // The general (capped) fetch: 1000+ rows, none of them run.complete —
    // exactly what a chatty run does to the oldest-first LIST_LIMIT window.
    const noise = Array.from({ length: 1000 }, (_, i) => ({
      id: `n${i}`,
      time: new Date().toISOString(),
      actor_type: "agent",
      actor: "agent",
      action: "egress.allow",
      target: "example.com",
      outcome: "success",
    }));
    listAuditMock.mockImplementation((_id: unknown, action?: string) => {
      if (action === "run.complete") {
        return Promise.resolve([
          {
            id: "complete-1",
            time: new Date().toISOString(),
            actor_type: "system",
            actor: "wardynd",
            action: "run.complete",
            target: "run-1",
            outcome: "success",
            data: { exit_code: 0 },
          },
        ]);
      }
      if (action === "run.kill" || action === "run.autostop" || action === "session.recording") {
        return Promise.resolve([]);
      }
      return Promise.resolve(noise);
    });
    renderRun({ ...RUN, state: "COMPLETED" });

    expect(await screen.findByText("exit 0")).toBeInTheDocument();
  });
});

// R-5: run.complete/run.kill/run.autostop cannot exist for a run that is not
// terminal yet — fetching them every DETAIL_POLL_MS tick on a live run was 3
// wasted round-trips per tick, forever.
describe("RunDetailScreen — R-5 the ending trio is skipped while the run is live", () => {
  it("never fetches run.complete/run.kill/run.autostop for a RUNNING run", async () => {
    renderRun({ ...RUN, state: "RUNNING" });
    await screen.findAllByText(RUN.task);
    expect(listAuditMock).not.toHaveBeenCalledWith("run-1", "run.complete");
    expect(listAuditMock).not.toHaveBeenCalledWith("run-1", "run.kill");
    expect(listAuditMock).not.toHaveBeenCalledWith("run-1", "run.autostop");
  });

  // Neg: a terminal run still gets them — same tick, off its own fresh state.
  it("neg: still fetches them the moment the run's own state is terminal", async () => {
    renderRun({ ...RUN, state: "COMPLETED" });
    await waitFor(() => expect(listAuditMock).toHaveBeenCalledWith("run-1", "run.complete"));
    expect(listAuditMock).toHaveBeenCalledWith("run-1", "run.kill");
    expect(listAuditMock).toHaveBeenCalledWith("run-1", "run.autostop");
  });
});

// R-6: F1-F2's generation counter advanced only on a NEW fetch starting,
// never on teardown — the finding's other half (setState after unmount) was
// still open. Fixed (run-detail.tsx: an unmount effect sets recRequest.current
// = -1), but deliberately UNPINNED: React 18 silently no-ops a state update
// against an unmounted fiber either way (the React 16/17 "Can't perform a
// React state update on an unmounted component" console warning this bug
// would have produced no longer exists), and calling the setter post-unmount
// throws nothing in either version either — tried both (an "unmount, resolve,
// assert no throw/no console.error" case) and confirmed BOTH pass identically
// with the fix reverted, i.e. no observable DOM/console/throw difference
// exists in this React version to red-first against. The fix is real defensive
// cleanup (matches the sibling pattern everywhere else in this file); it has
// no vacuous test standing in for a red-first pin.

// The point of the redesign, and therefore the assertion most worth pinning: a
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

  // #509 — isHeld's two unconditional arms (tool_call, credential_reauth)
  // (lib/types/approvals.ts) are shared by TWO call sites: the runs board
  // (board-groups.test.ts, runs/title-group.test.tsx) and this command bar,
  // via run-detail.tsx's `sandboxHeld={pending.some(isHeld)}`. Pinned here so
  // the cockpit keeps stating the hold at any age — the server parks the
  // agent on a PENDING row for up to WARDYN_APPROVAL_EXPIRY_AFTER (24h
  // default), and the console must not call it dead sooner.
  it("a PENDING tool_call 25 hours old still states 'sandbox held' in the command bar", async () => {
    listApprovalsMock.mockResolvedValue([
      {
        id: "a1",
        run_id: "run-1",
        kind: "tool_call",
        state: "PENDING",
        requested_at: new Date(Date.now() - 25 * 60 * 60_000).toISOString(),
        requested_scope: { tool: "Bash", cmd: "rm -rf build" },
      },
    ]);
    renderRun({ ...RUN, state: "WAITING_FOR_CONFIRMATION" });
    expect(await screen.findByText(/sandbox held/i)).toBeInTheDocument();
  });

  // Once the server has actually moved the row to EXPIRED, it is no longer
  // PENDING — the cockpit correctly stops claiming the sandbox is held.
  it("an EXPIRED tool_call no longer states 'sandbox held' in the command bar", async () => {
    listApprovalsMock.mockResolvedValue([
      {
        id: "a1",
        run_id: "run-1",
        kind: "tool_call",
        state: "EXPIRED",
        requested_at: new Date(Date.now() - 25 * 60 * 60_000).toISOString(),
        requested_scope: { tool: "Bash", cmd: "rm -rf build" },
      },
    ]);
    renderRun({ ...RUN, state: "WAITING_FOR_CONFIRMATION" });
    await screen.findByTestId("run-summary-header");
    expect(screen.queryByText(/sandbox held/i)).not.toBeInTheDocument();
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
describe("RunDetailScreen — failure-hint chip", () => {
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

// review U-01: onClone shares run-card.tsx's cloneRun refusal via
// cloneFromAudit — an unreadable run.create row (here, listAuditMock's own
// default: an empty trail) must toast and never navigate, exactly like the
// Runs-list kebab. Red-first: before the hoist, onClone read
// runPrefill(run, createRequestFromAudit(audit)) directly and always
// navigated, audit or no audit.
describe("RunDetailScreen — onClone refuses on an unreadable audit row (review U-01)", () => {
  it("toasts CLONE_UNREADABLE and does not navigate when the audit trail carries no run.create row", async () => {
    getRunMock.mockResolvedValue({ ...RUN, state: "COMPLETED" });
    const user = userEvent.setup();
    render(
      <MemoryRouter initialEntries={["/runs/run-1"]}>
        <Routes>
          <Route path="/runs/:id" element={<RunDetailScreen />} />
          <Route path="/runs/new" element={<div>NEW RUN PAGE</div>} />
        </Routes>
      </MemoryRouter>,
    );

    const cloneBtn = await screen.findByRole("button", { name: "Start a run like this one" });
    await user.click(cloneBtn);

    await waitFor(() =>
      expect(toast.warning).toHaveBeenCalledWith(
        "This run's launch settings couldn't be read — its clone would start from defaults, so it was not opened.",
      ),
    );
    expect(screen.queryByText("NEW RUN PAGE")).not.toBeInTheDocument();
  });
});

// The run-detail Audit tab's own fetch is capped at LIST_LIMIT
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
// drops a member on a feed that can never fill. M-1b: the full-page Audit
// screen is admin-only now, at /admin/audit.
describe("RunDetailScreen — open full Audit link", () => {
  it("carries the run id into /admin/audit", async () => {
    renderRun(RUN);
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    await user.click(await screen.findByRole("tab", { name: /audit/i }));

    expect(await screen.findByRole("link", { name: /open full audit/i })).toHaveAttribute(
      "href",
      "/admin/audit?run_id=run-1",
    );
  });

  it("neg: a user gets no link — the full Audit screen is Admin view only", async () => {
    getRunMock.mockResolvedValue(RUN);
    render(
      <MemoryRouter initialEntries={["/runs/run-1"]}>
        <OperatorProvider operator={false} securityOperator={false} principal="me">
          <Routes>
            <Route path="/runs/:id" element={<RunDetailScreen />} />
          </Routes>
        </OperatorProvider>
      </MemoryRouter>,
    );
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    await user.click(await screen.findByRole("tab", { name: /audit/i }));

    expect(await screen.findByRole("button", { name: /make a policy from this run/i })).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: /open full audit/i })).not.toBeInTheDocument();
  });
});

// M-1b: the Recordings library is Admin view only and hidden from a security
// admin until F1, so its link renders for an admin alone.
describe("RunDetailScreen — Recordings library link", () => {
  const CAST = { run_id: "run-1", header: { version: 2, width: 80, height: 24 }, events: [], cast: "" };

  function renderAs(operator: boolean, securityOperator: boolean) {
    getRunMock.mockResolvedValue({ ...RUN, state: "COMPLETED" });
    getRecordingMock.mockResolvedValue(CAST);
    render(
      <MemoryRouter initialEntries={["/runs/run-1"]}>
        <OperatorProvider operator={operator} securityOperator={securityOperator} principal="me">
          <Routes>
            <Route path="/runs/:id" element={<RunDetailScreen />} />
          </Routes>
        </OperatorProvider>
      </MemoryRouter>,
    );
  }

  it("an admin gets the link to /admin/recordings", async () => {
    renderAs(true, true);
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    await user.click(await screen.findByRole("tab", { name: /recording/i }));
    expect(await screen.findByRole("link", { name: "Recordings library" })).toHaveAttribute("href", "/admin/recordings");
  });

  it("neg: a security admin and a user get no link", async () => {
    for (const [op, sec] of [[false, true], [false, false]] as const) {
      renderAs(op, sec);
      const user = userEvent.setup({ pointerEventsCheck: 0 });
      await user.click(await screen.findByRole("tab", { name: /recording/i }));
      expect(await screen.findByText(/Recorded when the run's runner supports session capture/)).toBeInTheDocument();
      expect(screen.queryByRole("link", { name: "Recordings library" })).not.toBeInTheDocument();
      cleanup();
    }
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

// ui-member-cluster review sweep: canDecideApproval on this tab reads
// securityOperator (the SECURITY tier — admin OR security admin,
// authorizeUserDecision's early return), not plain isOperator — so the
// refusal chip must name SECURITY_ONLY_REASON, not OPERATOR_ONLY_REASON,
// which undersells who the gate actually admits.
describe("RunDetailScreen — the Approvals tab's decision gate names the security tier, not plain admin", () => {
  const toolCallApproval = {
    id: "a1",
    run_id: "run-1",
    kind: "tool_call",
    state: "PENDING",
    requested_at: new Date().toISOString(),
    requested_scope: { tool: "x" },
  };

  it("a security_admin (non-operator) decides a tool_call — no refusal chip at all", async () => {
    getRunMock.mockResolvedValue(RUN);
    listApprovalsMock.mockResolvedValue([toolCallApproval]);
    render(
      <MemoryRouter initialEntries={["/runs/run-1"]}>
        <OperatorProvider operator={false} securityOperator={true} principal="sec-admin">
          <Routes>
            <Route path="/runs/:id" element={<RunDetailScreen />} />
          </Routes>
        </OperatorProvider>
      </MemoryRouter>,
    );
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    await user.click(await screen.findByRole("tab", { name: /approvals/i }));

    expect(await screen.findByRole("button", { name: "Deny" })).not.toBeDisabled();
    expect(screen.queryByText(SECURITY_ONLY_REASON)).not.toBeInTheDocument();
  });

  // Neg: a plain member (neither operator nor security_admin) on the same
  // tool_call kind still gets refused — now naming BOTH roles that would work.
  it("neg: a plain member still gets refused, with the tier's own reason", async () => {
    getRunMock.mockResolvedValue(RUN);
    listApprovalsMock.mockResolvedValue([toolCallApproval]);
    render(
      <MemoryRouter initialEntries={["/runs/run-1"]}>
        <OperatorProvider operator={false} securityOperator={false} principal="someone-else">
          <Routes>
            <Route path="/runs/:id" element={<RunDetailScreen />} />
          </Routes>
        </OperatorProvider>
      </MemoryRouter>,
    );
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    await user.click(await screen.findByRole("tab", { name: /approvals/i }));

    expect(await screen.findByText(SECURITY_ONLY_REASON)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Deny" })).toBeDisabled();
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
