/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Split from run-detail.test.tsx (#195): that file was over the 800-line test
// gate. The hero-pane/cockpit-state and recording-selection describes stay
// there; the Approvals/Audit/Recordings-tab describes, the onClone refusal,
// the failure-hint chip and the failed-recording-fetch case live here.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
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
import { toast } from "sonner";

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
