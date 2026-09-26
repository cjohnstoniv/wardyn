/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Split from record-pane.test.tsx (#195): that file was over the 800-line
// test gate. The open-recording describes (header/banner, new session,
// open-record lifecycle, settled review) stay there; the confined-replay /
// verify-session describes and the role-gating describes that exercise them
// live here.
import { describe, it, expect, vi, type Mock } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { ProfileObservations, RecordResult, Workspace } from "../../../lib/types";
import { OperatorProvider } from "../../wardyn/operator-context";
import { OPERATOR_ONLY_REASON, SECURITY_ONLY_REASON } from "../../wardyn/copy";

// The embedded terminal is heavy (xterm) and irrelevant to what this pane decides,
// so stub it to a marker — we only assert it MOUNTS for a recording session.
vi.mock("../../attach-terminal", () => ({
  AttachTerminal: ({ runId }: { runId: string }) => <div data-testid="attach-terminal">{runId}</div>,
}));

// LiveApprovals (confined replay) polls the approvals API; mock it so the
// pane's live-approval widget is testable without a server.
const listApprovalsMock = vi.fn((..._a: unknown[]): Promise<unknown[]> => Promise.resolve([]));
const approveMock = vi.fn();
const denyMock = vi.fn();
vi.mock("../../../lib/api/approvals", () => ({
  approvals: {
    listApprovals: (...a: unknown[]) => listApprovalsMock(...a),
    approve: (...a: unknown[]) => approveMock(...a),
    deny: (...a: unknown[]) => denyMock(...a),
  },
}));
vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn(), info: vi.fn() } }));

import { RecordPane } from "./record-pane";

const obs = (over: Partial<ProfileObservations> = {}): ProfileObservations => ({
  domains: [],
  minted_grant_ids: [],
  exec_argv0s: [],
  file_writes: [],
  connects: [],
  anomalies: [],
  ...over,
});

function ws(over: Partial<Workspace> = {}): Workspace {
  return {
    id: "ws-1",
    name: "payments",
    kind: "repo",
    source: "acme/payments",
    status: "scanned",
    created_at: "",
    updated_at: "",
    ...over,
  };
}

const noop = () => {};
function renderPane(
  over: Partial<Workspace> = {},
  handlers: Partial<Record<string, Mock>> = {},
  modelReady = true,
  operator = true,
  launch: { warnings?: string[]; confinementClass?: string } | null = null,
  hostClasses: ("CC1" | "CC2" | "CC3")[] | null = null,
  // The two predicates are separate arguments, not ONE
  // (`securityOperator={operator}`) — collapsing them would let this helper
  // only ever produce admin (both true) or member (both false), making the
  // one tier 0.7 §B introduced, and the only one where the pane's split gate
  // is observable (operator:false, securityOperator:true), unreachable from
  // every test in this file. `securityOperator` defaults to `operator`, so
  // every existing call site keeps exactly the viewer it had.
  securityOperator = operator,
  // M-6/QM-10: which console view the not-ready model-access note reads for
  // — defaulted to User view (today's unchanged "set one up in Getting
  // started" copy) so every existing call site in this file keeps the exact
  // viewer it had; record-pane.test.tsx's own new Admin-view case passes
  // "/admin/workspaces/ws-1" explicitly.
  route = "/workspaces/ws-1",
) {
  return render(
    <MemoryRouter initialEntries={[route]}>
      {/* 0.7 §B: the pane moved to useSecurityOperator (record + promote-egress
          are securityOps), so the fixture's `operator=false` viewer must be a
          MEMBER on both predicates — a security admin CAN drive this pane, which
          is the point of the move. `securityOperator` defaults to `operator` so
          every existing caller is unchanged, and splits for the SECURITY-ADMIN
          persona (operator=false, securityOperator=true) — the caller F031 is
          about, whose approve-hosts write lands on operatorOnly. */}
      <OperatorProvider operator={operator} securityOperator={securityOperator}>
        <RecordPane
          ws={ws(over)}
          notice={null}
          launch={launch}
          busyTask={null}
          modelReady={modelReady}
          hostClasses={hostClasses}
          onRecord={handlers.onRecord ?? noop}
          onReplayConfined={handlers.onReplayConfined ?? noop}
          onDoneRecording={handlers.onDoneRecording ?? noop}
          onPromoteEgress={handlers.onPromoteEgress ?? noop}
          onApproveHosts={handlers.onApproveHosts ?? noop}
          onOpenProfile={handlers.onOpenProfile ?? noop}
        />
      </OperatorProvider>
    </MemoryRouter>,
  );
}

describe("RecordPane — empty capture is an honest failure, never success", () => {
  it("renders the reachability failure hint and offers no promotion", () => {
    const rr: RecordResult = {
      run_id: "r1",
      label: "build & test",
      mode: "interactive",
      status: "record_failed",
      failure_hint: "The sandbox couldn't reach the control plane (WSL2 NAT) to report egress decisions.",
    };
    renderPane({ record_results: { "build-test": rr } });
    const empty = screen.getByTestId("record-empty-capture");
    expect(empty).toHaveTextContent(/couldn't reach the control plane/i);
    expect(screen.queryByTestId("record-new-hosts")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /approve .* observed host/i })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: /re-record/i })).toBeInTheDocument();
  });
});

describe("RecordPane — confined replay (Replay confined -> replaying -> replayed, in place)", () => {
  // The recording made by the open session. Once it exists, its card offers
  // Replay confined — no separate name box, no separate step/page.
  const learning: RecordResult = { run_id: "o1", label: "build & test", mode: "interactive", status: "recorded" };
  // A settled CONFINED REPLAY of it, keyed verify:build-test: reached an allowed
  // host, an already-approved host, and an off-policy host BLOCKED (deny_count>0).
  const confinedRR: RecordResult = {
    run_id: "vr1",
    label: "build & test",
    mode: "interactive",
    confined: true,
    status: "recorded",
    observations: obs({
      domains: [
        { host: "registry.npmjs.org", allow_count: 4, deny_count: 0, pending_count: 0 },
        { host: "github.com", allow_count: 1, deny_count: 0, pending_count: 0 }, // already approved
        { host: "evil.example.com", allow_count: 0, deny_count: 2, pending_count: 0 }, // off-policy, blocked
      ],
    }),
  };

  it("shows the containment review once a recording's confined replay settles", async () => {
    const onApproveHosts = vi.fn();
    renderPane(
      {
        record_results: { "build-test": learning, "verify:build-test": confinedRR },
        approved_egress: ["github.com"],
      },
      { onApproveHosts },
    );
    const blocked = screen.getByTestId("verify-session-blocked");
    expect(within(blocked).getByText("evil.example.com")).toBeInTheDocument();
    // github.com is already approved → never shown as blocked/off-policy.
    expect(within(blocked).queryByText("github.com")).not.toBeInTheDocument();
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    // The per-host button survives the selectable list — one host, no replay.
    await user.click(within(blocked).getByRole("button", { name: /^approve$/i }));
    expect(onApproveHosts).toHaveBeenCalledWith(["evil.example.com"]);
    // Settled replay offers a re-run, in the SAME card. Exact-matched: the
    // guided "Approve N selected hosts and replay again" action also ends in
    // those two words.
    expect(screen.getByRole("button", { name: /^replay again$/i })).toBeInTheDocument();
  });

  // learnVerifyEgress writes a live approval to ws.requirements
  // (egress:<host>, level required), not ws.approved_egress — a host approved
  // that way must not still render as blocked, and the Approve CTA it would
  // duplicate-write with must not offer to approve it again.
  it("treats a host approved via the requirements contract (not approved_egress) as already-allowed", async () => {
    const rr: RecordResult = {
      ...confinedRR,
      observations: obs({
        domains: [
          { host: "registry.npmjs.org", allow_count: 4, deny_count: 0, pending_count: 0 },
          { host: "github.com", allow_count: 1, deny_count: 0, pending_count: 0 },
          { host: "learned.example.com", allow_count: 0, deny_count: 1, pending_count: 0 }, // live-approved via requirements
        ],
      }),
    };
    renderPane({
      record_results: { "build-test": learning, "verify:build-test": rr },
      approved_egress: ["github.com"],
      requirements: { "egress:learned.example.com": { level: "required", provenance: "operator_set" } },
    });
    const blocked = screen.queryByTestId("verify-session-blocked");
    expect(blocked === null || within(blocked).queryByText("learned.example.com") === null).toBe(true);
  });

  // The same overwrite-with-no-confirm gap, on the settled CONFINED review's
  // own re-run button.
  it("Replay again asks for confirmation before overwriting the settled containment review", async () => {
    const onReplayConfined = vi.fn();
    renderPane({ record_results: { "build-test": learning, "verify:build-test": confinedRR }, approved_egress: ["github.com"] }, { onReplayConfined });
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    await user.click(screen.getByRole("button", { name: /^replay again$/i }));
    expect(onReplayConfined).not.toHaveBeenCalled();
    const dialog = screen.getByRole("alertdialog");
    expect(within(dialog).getByText(/replaced once the new replay settles/i)).toBeInTheDocument();
    await user.click(within(dialog).getByRole("button", { name: /^replay again$/i }));
    expect(onReplayConfined).toHaveBeenCalledWith("build & test");
  });

  it("embeds the attach terminal and live approvals while a confined replay is recording", () => {
    const running: RecordResult = { run_id: "vr9", label: "build & test", mode: "interactive", confined: true, status: "recording" };
    renderPane({ record_results: { "build-test": learning, "verify:build-test": running } });
    expect(screen.getByTestId("attach-terminal")).toHaveTextContent("vr9");
  });

  it("surfaces the run's pending off-policy approval live and denies it inline", async () => {
    listApprovalsMock.mockReset();
    denyMock.mockReset();
    listApprovalsMock.mockResolvedValue([
      { id: "apr1", run_id: "vr9", kind: "egress_domain", requested_scope: { host: "evil.example.com" }, state: "PENDING", requested_at: "" },
      { id: "aprX", run_id: "other", kind: "egress_domain", requested_scope: { host: "other.com" }, state: "PENDING", requested_at: "" },
    ]);
    const running: RecordResult = { run_id: "vr9", label: "build & test", mode: "interactive", confined: true, status: "recording" };
    renderPane({ record_results: { "build-test": learning, "verify:build-test": running } });

    // Only THIS run's pending approval shows (the other run's is filtered out).
    const panel = await screen.findByTestId("live-approvals");
    expect(within(panel).getByText("evil.example.com")).toBeInTheDocument();
    expect(within(panel).queryByText("other.com")).not.toBeInTheDocument();

    // Deny is a two-step confirm (LiveApprovals: it
    // permanently poisons the host, so a click opens a confirm dialog rather
    // than calling the API directly — see live-approvals.test.tsx's own
    // "Deny opens a confirm dialog" pin) — click the row's Deny, then the
    // dialog's own Deny action.
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    await user.click(within(panel).getByRole("button", { name: /^deny$/i }));
    const dialog = await screen.findByRole("alertdialog");
    await user.click(within(dialog).getByRole("button", { name: /^deny$/i }));
    expect(denyMock).toHaveBeenCalledWith("apr1", expect.any(String));
  });

  it("Done on a replaying session calls onDoneRecording with the CONFINED run id", async () => {
    const onDoneRecording = vi.fn();
    const running: RecordResult = { run_id: "vr9", label: "build & test", mode: "interactive", confined: true, status: "recording" };
    renderPane({ record_results: { "build-test": learning, "verify:build-test": running } }, { onDoneRecording });
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    await user.click(screen.getByRole("button", { name: /^done$/i }));
    expect(onDoneRecording).toHaveBeenCalledWith("vr9");
  });

  it("disables Replay confined only for the busy session (busyTask keys off verifyKeyOf)", () => {
    render(
      <MemoryRouter>
        <RecordPane
          ws={ws({ record_results: { "build-test": learning } })}
          notice={null}
          busyTask="verify:build-test"
          modelReady
          onRecord={noop}
          onReplayConfined={noop}
          onDoneRecording={noop}
          onPromoteEgress={noop}
          onApproveHosts={noop}
          onOpenProfile={noop}
        />
      </MemoryRouter>,
    );
    expect(screen.getByRole("button", { name: /^replay confined$/i })).toBeDisabled();
    expect(screen.getByRole("button", { name: /re-record/i })).toBeEnabled();
  });
});

// B4: the settled-CONFINED chip's four verdict branches (replayedChipMeta) —
// B2 shipped the logic but pinned none of these; the earlier "confined
// replay" describe above only ever exercises the caught (deny) case.
describe("RecordPane — replayed chip verdict (clean/caught)", () => {
  const learningRR: RecordResult = { run_id: "o1", label: "build & test", mode: "interactive", status: "recorded" };
  const confined = (over: Partial<RecordResult> = {}): RecordResult => ({
    run_id: "vr1",
    label: "build & test",
    mode: "interactive",
    confined: true,
    status: "recorded",
    observations: obs(),
    ...over,
  });

  it("clean===true renders the green 'Replayed clean' chip", () => {
    renderPane({ record_results: { "build-test": learningRR, "verify:build-test": confined({ clean: true, caught: 0 }) } });
    expect(within(screen.getByTestId("session-build-test")).getByText("Replayed clean")).toBeInTheDocument();
  });

  it("clean===false with caught>0 renders 'Replayed — caught N'", () => {
    renderPane({ record_results: { "build-test": learningRR, "verify:build-test": confined({ clean: false, caught: 2 }) } });
    expect(within(screen.getByTestId("session-build-test")).getByText("Replayed — caught 2")).toBeInTheDocument();
  });

  // clean can fail with ZERO caught hosts too: a live mid-replay approval
  // release, or a truncated capture — neither is a denied/held host, so the
  // wording must not claim a count of zero.
  it("clean===false with caught 0 renders the honest 'Replayed — not clean' wording", () => {
    renderPane({ record_results: { "build-test": learningRR, "verify:build-test": confined({ clean: false, caught: 0 }) } });
    expect(within(screen.getByTestId("session-build-test")).getByText("Replayed — not clean")).toBeInTheDocument();
  });

  it("clean absent (a pre-Workstream-B row) renders the neutral legacy 'Replayed confined' chip", () => {
    renderPane({ record_results: { "build-test": learningRR, "verify:build-test": confined() } });
    expect(within(screen.getByTestId("session-build-test")).getByText("Replayed confined")).toBeInTheDocument();
  });
});

// B4: the guided selector's per-host default — a footgun the moment ANY
// caught host is a genuine villain (ep 09's own scenario), so a live-denied
// host must default OUT of the batch while a merely-held one defaults in.
describe("RecordPane — guided approve: caught-host checkbox defaults", () => {
  it("a live-denied host starts unchecked; a merely-held host starts checked", () => {
    const learningRR: RecordResult = { run_id: "o1", label: "build & test", mode: "interactive", status: "recorded" };
    const confinedRR: RecordResult = {
      run_id: "vr1",
      label: "build & test",
      mode: "interactive",
      confined: true,
      status: "recorded",
      observations: obs({
        domains: [
          { host: "evil.example.com", allow_count: 0, deny_count: 1, pending_count: 0 }, // denied live
          { host: "files.pythonhosted.org", allow_count: 0, deny_count: 0, pending_count: 1 }, // held only
        ],
      }),
    };
    renderPane({ record_results: { "build-test": learningRR, "verify:build-test": confinedRR } });
    const blocked = screen.getByTestId("verify-session-blocked");
    expect(within(blocked).getByRole("checkbox", { name: "Approve evil.example.com" })).not.toBeChecked();
    expect(within(blocked).getByRole("checkbox", { name: "Approve files.pythonhosted.org" })).toBeChecked();
    // The guided button's own count follows the default selection: the held
    // host only — bulk-approving the denied one too would be the exact
    // footgun the default exists to prevent.
    expect(screen.getByRole("button", { name: /^approve 1 selected host and replay again$/i })).toBeInTheDocument();
  });
});

// B4 / root-cause bucket fix (B2): ConfinedReviewCard's caught bucket must
// subtract the SAME union egressPromotionDiff does (approvedEgressSet: legacy
// ApprovedEgress + profile.egress_domains + effective egress:<host>
// requirement rows) — not ws.approved_egress alone, or a host approved
// through the requirements lane (promote, and the per-host approve since B2)
// keeps rendering as off-policy under a button that would fold in nothing new.
describe("RecordPane — off-policy bucket subtracts the requirements lane too", () => {
  it("a host approved via effective_requirements (not legacy approved_egress) no longer buckets as blocked", () => {
    const learningRR: RecordResult = { run_id: "o1", label: "build & test", mode: "interactive", status: "recorded" };
    const confinedRR: RecordResult = {
      run_id: "vr1",
      label: "build & test",
      mode: "interactive",
      confined: true,
      status: "recorded",
      observations: obs({
        domains: [{ host: "registry.npmjs.org", allow_count: 0, deny_count: 1, pending_count: 0 }],
      }),
    };
    renderPane({
      record_results: { "build-test": learningRR, "verify:build-test": confinedRR },
      effective_requirements: { "egress:registry.npmjs.org": { level: "required", provenance: "operator_set" } },
    });
    // The chip still reports the replay's own history (caught it), but the
    // requirements-lane approval already covers the host, so the bucket that
    // drives the guided "approve + replay" action must not offer it again.
    expect(screen.queryByTestId("verify-session-blocked")).not.toBeInTheDocument();
  });
});

// A wizard Verify session is stored confined under key "verify:verify" with
// no open "verify" sibling — recordSessions (open-only) can never list it, so
// without orphanedVerifySessions + this card it would be invisible here even
// while isRecording(ws) still counts it: "Never recorded", "Start recording"
// disabled, no way to see or stop the live run.
describe("RecordPane — an orphaned verify:* session (no open sibling)", () => {
  it("live: renders a stoppable card (terminal + live approvals + Done), never 'Never recorded'", async () => {
    const onDoneRecording = vi.fn();
    const running: RecordResult = { run_id: "vr-live", label: "verify", mode: "interactive", confined: true, status: "recording" };
    renderPane({ record_results: { "verify:verify": running } }, { onDoneRecording });

    expect(screen.queryByText(/never recorded/i)).not.toBeInTheDocument();
    expect(screen.getByTestId("session-verify:verify")).toBeInTheDocument();
    expect(screen.getByTestId("attach-terminal")).toHaveTextContent("vr-live");

    const user = userEvent.setup({ pointerEventsCheck: 0 });
    await user.click(screen.getByRole("button", { name: /^done$/i }));
    expect(onDoneRecording).toHaveBeenCalledWith("vr-live");
  });

  it("settled: renders the containment review (allowed/blocked), same as a named session's confined replay", () => {
    const settled: RecordResult = {
      run_id: "vr-done",
      label: "verify",
      mode: "interactive",
      confined: true,
      status: "recorded",
      observations: obs({
        domains: [
          { host: "registry.npmjs.org", allow_count: 3, deny_count: 0, pending_count: 0 },
          { host: "evil.example.com", allow_count: 0, deny_count: 1, pending_count: 0 },
        ],
      }),
    };
    renderPane({ record_results: { "verify:verify": settled } });

    expect(screen.queryByText(/never recorded/i)).not.toBeInTheDocument();
    expect(screen.queryByTestId("attach-terminal")).not.toBeInTheDocument();
    const blocked = screen.getByTestId("verify-session-blocked");
    expect(within(blocked).getByText("evil.example.com")).toBeInTheDocument();
  });

  it("a named session's OWN confined replay is not treated as orphaned (its open sibling exists)", () => {
    const learning: RecordResult = { run_id: "o1", label: "build & test", mode: "interactive", status: "recorded" };
    const confinedRR: RecordResult = { run_id: "vr1", label: "build & test", mode: "interactive", confined: true, status: "recorded" };
    renderPane({ record_results: { "build-test": learning, "verify:build-test": confinedRR } });

    // One session card (the named one, in its own "replayed" stage) — no
    // second, orphan-shaped card for the same confined result.
    expect(screen.queryByTestId("record-orphaned-sessions")).not.toBeInTheDocument();
  });
});

// securityOps at the server for every control here (record/replay/approve-
// host/promote-egress) — a viewer must see them disabled, not enabled-then-403.
describe("RecordPane — a viewer's controls are disabled", () => {
  // 0.7 §B, as amended by R1: a SECURITY ADMIN (operator:false,
  // security_operator:true) keeps the DECISION half of this pane —
  // .../promote-egress and the approve-hosts PUT .../approved-egress are still
  // securityOps, and gating the pane on useOperator would show them a dead pane
  // over routes the server honours. The LAUNCH half moved: POST
  // /workspaces/{id}/record is operatorOnly, because it starts a credentialed,
  // host-mounting, open-egress sandbox and stamps the caller as its owner
  // rather than deciding an egress question. So the split, not the whole pane,
  // is what this asserts — enabled-then-403 in EITHER direction is the bug.
  it("splits a security admin's controls: the decision half live, the launch half disabled", () => {
    // A session with an OBSERVED host the workspace has not approved, so the
    // decision control (Approve N observed hosts -> promote-egress) actually
    // renders — without it this test could only prove the launch half.
    const recorded: RecordResult = {
      run_id: "r1",
      label: "build & test",
      mode: "interactive",
      status: "recorded",
      observations: {
        domains: [{ host: "registry.npmjs.org", allow_count: 4, deny_count: 0, pending_count: 0 }],
      } as unknown as RecordResult["observations"],
    };
    // Through renderPane, not a hand-rolled render: the helper is the file's
    // one harness, and a tier only one bespoke block can reach is a tier the
    // other 800 lines silently cannot test.
    renderPane({ record_results: { "build-test": recorded } }, {}, true, false, null, null, true);
    // The DECISION half — promoting observed egress into the allowlist — is
    // theirs and stays live. This is what makes the pane worth showing them at
    // all, and what a blanket useOperator gate would have taken away.
    expect(screen.getByRole("button", { name: /approve 1 observed host/i })).not.toBeDisabled();
    // The LAUNCH half is not: all three of these POST .../record, which is
    // operatorOnly.
    expect(screen.getByRole("button", { name: /^replay confined$/i })).toBeDisabled();
    expect(screen.getByRole("button", { name: /re-record/i })).toBeDisabled();
    expect(screen.getByLabelText(/session name/i)).toBeDisabled();
    // The reason is stated where the dead control is (NewSessionForm's own
    // note) and NOT at pane level — the pane itself is theirs to drive.
    expect(screen.getByTestId("record-new-session")).toHaveTextContent(OPERATOR_ONLY_REASON);
    expect(screen.getAllByText(OPERATOR_ONLY_REASON)).toHaveLength(1);
  });

  // The other operatorOnly control on this pane, at the same tier: an orphaned
  // confined session's Done button. Reachable only now that the helper splits
  // the predicates.
  it("leaves a security admin the orphaned session's Done button — it is inside the securityOps fieldset", () => {
    const running: RecordResult = { run_id: "vr-live", label: "verify", mode: "interactive", confined: true, status: "recording" };
    renderPane({ record_results: { "verify:verify": running } }, {}, true, false, null, null, true);
    expect(screen.getByRole("button", { name: /^done$/i })).not.toBeDisabled();
  });

  // The mirror image: a plain MEMBER (both predicates false) sees the whole
  // pane dead and the reason once, at pane level.
  it("gives a plain member a dead pane with the reason stated once, at pane level", () => {
    const recorded: RecordResult = { run_id: "r1", label: "build & test", mode: "interactive", status: "recorded" };
    renderPane({ record_results: { "build-test": recorded } }, {}, true, false, null, null, false);
    expect(screen.getByLabelText(/session name/i)).toBeDisabled();
    expect(screen.getByRole("button", { name: /^replay confined$/i })).toBeDisabled();
    expect(screen.getByText(SECURITY_ONLY_REASON)).toBeInTheDocument();
    expect(screen.getByTestId("record-new-session")).toHaveTextContent(OPERATOR_ONLY_REASON);
    expect(screen.getAllByText(OPERATOR_ONLY_REASON)).toHaveLength(1);
  });

  it("disables the New-session field, Start recording, and a session's Replay/Re-record buttons", () => {
    const recorded: RecordResult = { run_id: "r1", label: "build & test", mode: "interactive", status: "recorded" };
    renderPane({ record_results: { "build-test": recorded } }, {}, true, false);

    // The name field's own disabled attribute proves the fieldset gate
    // directly — "Start recording" is also disabled by its own empty-name
    // condition regardless, so it alone wouldn't prove much.
    expect(screen.getByLabelText(/session name/i)).toBeDisabled();
    expect(screen.getByRole("button", { name: /start recording/i })).toBeDisabled();
    expect(screen.getByRole("button", { name: /^replay confined$/i })).toBeDisabled();
    expect(screen.getByRole("button", { name: /re-record/i })).toBeDisabled();
  });

  it("disables the orphaned session's Done button too", () => {
    const running: RecordResult = { run_id: "vr-live", label: "verify", mode: "interactive", confined: true, status: "recording" };
    renderPane({ record_results: { "verify:verify": running } }, {}, true, false);
    expect(screen.getByRole("button", { name: /^done$/i })).toBeDisabled();
  });
});

// F031 — the pane opens to a security admin (its fieldset gates on the
// securityOps tier), but the Approve controls call approveHosts, whose PUT
// .../requirements is registered on operatorOnly. They must carry their own
// gate, or a security admin gets live buttons over a route the server refuses
// and the guided approve->replay chain silently never fires its replay.
describe("RecordPane — approve-hosts is operatorOnly, above the pane's own tier", () => {
  const learningRR: RecordResult = { run_id: "o1", label: "build & test", mode: "interactive", status: "recorded" };
  const confinedRR: RecordResult = {
    run_id: "vr1",
    label: "build & test",
    mode: "interactive",
    confined: true,
    status: "recorded",
    observations: obs({
      domains: [{ host: "evil.example.com", allow_count: 0, deny_count: 0, pending_count: 1 }],
    }),
  };
  const caughtWs = { record_results: { "build-test": learningRR, "verify:build-test": confinedRR } };

  it("disables both Approve controls for a security admin, and says which role they need", () => {
    // operator=false, securityOperator=true — the security-admin caller.
    renderPane(caughtWs, {}, true, false, null, null, true);
    const blocked = screen.getByTestId("verify-session-blocked");
    // The pane itself is OPEN to this caller (its fieldset is keyed on the
    // security tier) — proven by a sibling control that stays live.
    expect(within(blocked).getByRole("checkbox", { name: "Approve evil.example.com" })).toBeEnabled();

    const perHost = within(blocked).getByRole("button", { name: /^approve$/i });
    expect(perHost).toBeDisabled();
    const guided = screen.getByRole("button", { name: /^approve 1 selected host and replay again$/i });
    expect(guided).toBeDisabled();
    // The reason is now visible text beside/under each control, not a title
    // tooltip — one per-host trailing hint, one standalone helper line.
    expect(within(blocked).getAllByText(OPERATOR_ONLY_REASON)).toHaveLength(2);
  });

  it("leaves both Approve controls live for an admin, who holds the requirements tier", () => {
    renderPane(caughtWs);
    const blocked = screen.getByTestId("verify-session-blocked");
    expect(within(blocked).getByRole("button", { name: /^approve$/i })).toBeEnabled();
    expect(screen.getByRole("button", { name: /^approve 1 selected host and replay again$/i })).toBeEnabled();
  });
});
