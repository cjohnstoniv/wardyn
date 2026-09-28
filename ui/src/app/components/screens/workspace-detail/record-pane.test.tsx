/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The pane's per-session lifecycle has no separate confined/replayMode prop —
// each session's card shows whichever stage it's actually in: record open ->
// recorded -> [Replay confined] -> replaying -> replayed.
import { describe, it, expect, vi, beforeEach, type Mock } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { ProfileObservations, RecordResult, Workspace } from "../../../lib/types";
import { OperatorProvider } from "../../wardyn/operator-context";

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

beforeEach(() => {
  localStorage.clear();
});

describe("RecordPane — header, open-egress banner, model note", () => {
  it("shows the recommended/skippable chip and, with no tier data at all, the weakest-barrier wording", () => {
    renderPane();
    expect(screen.getByText(/recommended/i)).toBeInTheDocument();
    const banner = screen.getByTestId("record-open-egress-banner");
    expect(banner).toHaveTextContent(/fence/i);
    expect(banner).toHaveTextContent(/egress/i);
  });

  // The banner NEVER keys off the operator's New-Run default (localStorage) —
  // that preference once printed "Fence" under a Vault capture, on camera. The
  // pre-launch tier comes from the runner's declared classes: a recording
  // launches under the strongest of them (workspace_run.go's bestClass).
  it("derives the pre-launch tier from the runner's strongest class, not localStorage", () => {
    localStorage.setItem("wardyn-default-confinement", "CC1"); // must be ignored
    renderPane({}, {}, true, true, null, ["CC1", "CC2", "CC3"]);
    const banner = screen.getByTestId("record-open-egress-banner");
    expect(banner).toHaveTextContent(/egress unrestricted/i);
    expect(banner).not.toHaveTextContent(/weakest barrier/i);
  });

  it("notes the configured model provider when a model path is ready", () => {
    renderPane({}, {}, true);
    expect(screen.getByText(/configured model provider/i)).toBeInTheDocument();
  });

  it("warns when no model path is ready", () => {
    renderPane({}, {}, false);
    expect(screen.getByText(/no model provider is configured/i)).toBeInTheDocument();
  });

  // M-6 (QM-10/§4.6): in the Admin view the not-ready line states the REAL
  // dependency (the admin's own connection, made in the User view) instead
  // of pointing at Admin-view Getting Started, which cannot configure it.
  it("in the Admin view, states the Record-runs-on-your-own-connection dependency and links to User view", () => {
    renderPane({}, {}, false, true, null, null, true, "/admin/workspaces/ws-1");
    expect(
      screen.getByText("Record uses your own model connection — connect it in the user view."),
    ).toBeInTheDocument();
    expect(screen.queryByText(/no model provider is configured/i)).not.toBeInTheDocument();
    const link = screen.getByRole("link", { name: "Open in user view" });
    expect(link).toHaveAttribute("href", "/account");
  });

  // Once a session has actually launched, the server's own
  // confinement_class is the truth — it beats the runner-derived guess in
  // BOTH directions (stronger: the weakest-barrier line goes; weaker: it
  // appears even though the runner offers better).
  it("keys the tier line off the launch's REAL confinement class once a session has launched", () => {
    renderPane({}, {}, true, true, { confinementClass: "CC3" });
    expect(screen.getByTestId("record-open-egress-banner")).not.toHaveTextContent(/weakest barrier/i);
  });

  it("shows the weakest-barrier line when the launch really resolved to CC1, even on a stronger host", () => {
    renderPane({}, {}, true, true, { confinementClass: "CC1" }, ["CC1", "CC2", "CC3"]);
    expect(screen.getByTestId("record-open-egress-banner")).toHaveTextContent(/weakest barrier/i);
  });

  it("renders the launch's own warnings instead of silently dropping them", () => {
    renderPane({}, {}, true, true, {
      confinementClass: "CC1",
      warnings: ["this recording runs with OPEN egress under the WEAKEST available isolation"],
    });
    const warn = screen.getByTestId("record-launch-warnings");
    expect(warn).toHaveTextContent(/OPEN egress/);
  });
});

describe("RecordPane — new session", () => {
  it("empty state suggests a name and starts a session with the typed name", async () => {
    const onRecord = vi.fn();
    renderPane({}, { onRecord });
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    const form = screen.getByTestId("record-new-session");
    // Default suggestion for the empty state.
    expect(within(form).getByLabelText(/session name/i)).toHaveValue("build & test");
    await user.click(within(form).getByRole("button", { name: /start recording/i }));
    expect(onRecord).toHaveBeenCalledWith("build & test");
  });

  it("passes the operator's own session name verbatim", async () => {
    const onRecord = vi.fn();
    renderPane({}, { onRecord });
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    const input = within(screen.getByTestId("record-new-session")).getByLabelText(/session name/i);
    await user.clear(input);
    await user.type(input, "agent dev loop");
    await user.click(screen.getByRole("button", { name: /start recording/i }));
    expect(onRecord).toHaveBeenCalledWith("agent dev loop");
  });

  it("shows the never-recorded helper line when there are no sessions yet", () => {
    renderPane();
    expect(screen.getByText(/never recorded/i)).toBeInTheDocument();
  });
});

describe("RecordPane — open-record session lifecycle", () => {
  it("a recording session embeds the attach terminal, shows detected-command hints, and Done", async () => {
    const onDoneRecording = vi.fn();
    const rr: RecordResult = { run_id: "run-42", label: "build & test", mode: "interactive", status: "recording" };
    renderPane(
      {
        record_results: { "build-test": rr },
        profile: { setup_commands: [{ stage: "install", command: "npm ci" }] },
      },
      { onDoneRecording },
    );
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    expect(screen.getByTestId("attach-terminal")).toHaveTextContent("run-42");
    expect(screen.getByText("npm ci")).toBeInTheDocument(); // detected-command hint pill
    // Sessions survive navigation — this note is the promise, made while live.
    expect(screen.getByText(/keeps running until you click done/i)).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: /done recording/i }));
    expect(onDoneRecording).toHaveBeenCalledWith("run-42");
  });

  // The attach chokepoint does not refuse a run whose creator it cannot see
  // (P1), which is right for the login pane — but this pane's sessions belong to
  // the OPERATOR who launched them (record/replay are operatorOnly, routes.go),
  // so a member merely VIEWING this workspace never owns the run. Mounting a
  // terminal for them buys a failed ticket mint, a raw "could not mint an
  // attach ticket" error instead of the role sentence, and one
  // authz.denied{not_owner} row per mount — the terminal must be
  // operator-gated the same as the pane's other controls.
  it("a non-operator sees no terminal for a recording session — only the operator gets one", () => {
    const rr: RecordResult = { run_id: "run-42", label: "build & test", mode: "interactive", status: "recording" };
    renderPane({ record_results: { "build-test": rr } }, {}, true, false);
    expect(screen.queryByTestId("attach-terminal")).not.toBeInTheDocument();
    // The session itself is still visible — this hides a control they never had,
    // not the fact that a recording is running.
    expect(screen.getByTestId("session-build-test")).toBeInTheDocument();
  });

  it("survives an unmount — no kill-on-unmount call for an in-flight session", () => {
    const onDoneRecording = vi.fn();
    const rr: RecordResult = { run_id: "run-42", label: "build & test", mode: "interactive", status: "recording" };
    const { unmount } = renderPane({ record_results: { "build-test": rr } }, { onDoneRecording });
    unmount();
    // Nothing kills the run just because the page/pane went away.
    expect(onDoneRecording).not.toHaveBeenCalled();
  });
});

describe("RecordPane — settled review card (open recording)", () => {
  const recorded = (over: Partial<RecordResult> = {}): RecordResult => ({
    run_id: "r1",
    label: "build & test",
    mode: "interactive",
    status: "recorded",
    observations: obs({
      domains: [
        { host: "registry.npmjs.org", allow_count: 4, deny_count: 0, pending_count: 0 },
        { host: "github.com", allow_count: 1, deny_count: 0, pending_count: 0 }, // already approved
      ],
    }),
    secret_names_minted: ["DATABASE_URL"],
    ...over,
  });

  const profile = {
    required_secrets: [{ name: "DATABASE_URL" }, { name: "STRIPE_KEY" }],
  } as unknown as Record<string, unknown>;

  it("offers to approve the newly-observed host and calls onPromoteEgress with the session key", async () => {
    const onPromoteEgress = vi.fn();
    renderPane(
      { record_results: { "build-test": recorded() }, approved_egress: ["github.com"], profile },
      { onPromoteEgress },
    );
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    const review = screen.getByTestId("record-review");
    expect(within(review).getByTestId("record-new-hosts")).toHaveTextContent("registry.npmjs.org");
    await user.click(within(review).getByRole("button", { name: /approve 1 observed host/i }));
    expect(onPromoteEgress).toHaveBeenCalledWith("build-test");
  });

  // egress_promoted is a boolean the server flips on ANY promoted>0, so the
  // all-done "Promoted" chip is only honest when NOTHING is still approvable —
  // hence both observed hosts approved here. (A partial promote keeps its list
  // and its Approve button; that branch is B4's.)
  it("shows a Promoted badge (no approve button) once every observed host is promoted", () => {
    renderPane({
      record_results: { "build-test": recorded({ egress_promoted: true }) },
      approved_egress: ["registry.npmjs.org", "github.com"],
      profile,
    });
    const review = screen.getByTestId("record-review");
    expect(within(review).getByText(/^promoted$/i)).toBeInTheDocument();
    expect(within(review).queryByRole("button", { name: /approve .* observed host/i })).not.toBeInTheDocument();
  });

  // B4: the K>0 counterpart above — egress_promoted is a boolean the server
  // flips on ANY promoted>0, so a promote that landed SOME but not all of what
  // was observed must say so honestly (never a blanket "Promoted"), and the
  // remainder keeps BOTH its list and its Approve button (the all-done branch
  // above hides both).
  it("partial promote: 'Promoted — K still need approval' keeps the remaining host's list and Approve button", async () => {
    const onPromoteEgress = vi.fn();
    renderPane(
      { record_results: { "build-test": recorded({ egress_promoted: true }) }, approved_egress: ["github.com"], profile },
      { onPromoteEgress },
    );
    const review = screen.getByTestId("record-review");
    expect(within(review).getByText("Promoted — 1 still needs approval")).toBeInTheDocument();
    expect(within(review).getByTestId("record-new-hosts")).toHaveTextContent("registry.npmjs.org");
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    await user.click(within(review).getByRole("button", { name: /approve 1 observed host/i }));
    expect(onPromoteEgress).toHaveBeenCalledWith("build-test");
  });

  // An observed-and-allowed host that is platform plumbing (the
  // model-provider harness host every session needs, or the console's own
  // origin) must never be offered for approval, and — since nothing needed
  // approving at all — the pane must NOT claim "already allowed" (that
  // implies an operator decision that never happened) or "Promoted" (nothing
  // was ever promoted; egress_promoted stays unset).
  describe("plumbing hosts are never offered for approval", () => {
    const plumbingOnly = (over: Partial<RecordResult> = {}): RecordResult => ({
      run_id: "r1",
      label: "build & test",
      mode: "interactive",
      status: "recorded",
      observations: obs({
        domains: [{ host: "api.anthropic.com", allow_count: 5, deny_count: 0, pending_count: 0 }],
      }),
      ...over,
    });

    it("model-provider host: no approve button, honest 'platform plumbing' message, no false 'already allowed'", () => {
      renderPane({ record_results: { "build-test": plumbingOnly() }, profile });
      const review = screen.getByTestId("record-review");
      expect(within(review).queryByRole("button", { name: /approve .* observed host/i })).not.toBeInTheDocument();
      expect(within(review).queryByText(/promoted/i)).not.toBeInTheDocument();
      expect(within(review).getByText(/platform plumbing/i)).toBeInTheDocument();
      expect(within(review).queryByText(/already allowed/i)).not.toBeInTheDocument();
      expect(within(review).queryByLabelText("Already approved")).not.toBeInTheDocument();
    });

    it("the console's own origin (window.location.hostname) is excluded the same way", () => {
      renderPane({
        record_results: {
          "build-test": plumbingOnly({
            observations: obs({
              domains: [{ host: window.location.hostname, allow_count: 2, deny_count: 0, pending_count: 0 }],
            }),
          }),
        },
        profile,
      });
      const review = screen.getByTestId("record-review");
      expect(within(review).queryByRole("button", { name: /approve .* observed host/i })).not.toBeInTheDocument();
      expect(within(review).getByText(/platform plumbing/i)).toBeInTheDocument();
    });

    it("a mix of a real host and a plumbing host still offers only the real one, correctly counted", async () => {
      const onPromoteEgress = vi.fn();
      renderPane(
        {
          record_results: {
            "build-test": plumbingOnly({
              observations: obs({
                domains: [
                  { host: "api.anthropic.com", allow_count: 5, deny_count: 0, pending_count: 0 },
                  { host: "registry.npmjs.org", allow_count: 2, deny_count: 0, pending_count: 0 },
                ],
              }),
            }),
          },
          profile,
        },
        { onPromoteEgress },
      );
      const review = screen.getByTestId("record-review");
      const list = within(review).getByTestId("record-new-hosts");
      expect(list).toHaveTextContent("registry.npmjs.org");
      expect(list).not.toHaveTextContent("api.anthropic.com");
      // The count in the button label must match — the exact "click Approve N
      // but the server only promotes fewer" mismatch this finding closes.
      expect(within(review).getByRole("button", { name: /approve 1 observed host/i })).toBeInTheDocument();
      await userEvent.setup({ pointerEventsCheck: 0 }).click(within(review).getByRole("button", { name: /approve 1 observed host/i }));
      expect(onPromoteEgress).toHaveBeenCalledWith("build-test");
    });
  });

  it("chips only the declared secrets that were actually minted (proven used)", () => {
    renderPane({ record_results: { "build-test": recorded() }, profile });
    const chips = screen.getByTestId("record-proven-secrets");
    expect(within(chips).getByText("DATABASE_URL")).toBeInTheDocument();
    expect(within(chips).queryByText("STRIPE_KEY")).not.toBeInTheDocument();
  });

  it("opens the ProfileReview drawer on the record run when Save is clicked", async () => {
    const onOpenProfile = vi.fn();
    renderPane({ record_results: { "build-test": recorded() }, profile }, { onOpenProfile });
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    await user.click(screen.getByRole("button", { name: /save .*profile/i }));
    expect(onOpenProfile).toHaveBeenCalledWith("r1", "payments-build-test");
  });

  it("renders a PARTIAL capture (backend omits empty arrays) without crashing", () => {
    // Real captures drop empty arrays (Go omitempty): only domains + minted_grant_ids
    // came back here. The review card + Observations must not read undefined.length.
    const rr: RecordResult = {
      run_id: "r1",
      label: "build & test",
      mode: "interactive",
      status: "recorded",
      observations: {
        domains: [{ host: "registry.npmjs.org", allow_count: 4, deny_count: 0, pending_count: 0 }],
        minted_grant_ids: [],
      } as unknown as RecordResult["observations"],
    };
    renderPane({ record_results: { "build-test": rr }, approved_egress: [], profile });
    expect(screen.getByTestId("record-review")).toBeInTheDocument();
    expect(screen.getByLabelText("Observations")).toBeInTheDocument();
  });

  it("renders the standing masking caveat and a CC3 sensor-blind note when blind", () => {
    renderPane({ record_results: { "build-test": recorded({ kernel_sensor_blind: true }) }, profile });
    const review = screen.getByTestId("record-review");
    expect(within(review).getByText(/masking is seed-ahead/i)).toBeInTheDocument();
    expect(within(review).getByText(/syscall sensor can't see/i)).toBeInTheDocument();
  });

  it("offers Replay confined once a recording settles, calling onReplayConfined with its label", async () => {
    const onReplayConfined = vi.fn();
    renderPane({ record_results: { "build-test": recorded() }, profile }, { onReplayConfined });
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    await user.click(screen.getByRole("button", { name: /^replay confined$/i }));
    expect(onReplayConfined).toHaveBeenCalledWith("build & test");
  });

  it("does not offer Replay confined for a failed open capture", () => {
    renderPane({ record_results: { "build-test": recorded({ status: "record_failed" }) } });
    expect(screen.queryByRole("button", { name: /^replay confined$/i })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: /re-record/i })).toBeInTheDocument();
  });

  // Re-record must not fire the launch on the first click — it would
  // silently replace the settled review this card is showing.
  it("Re-record asks for confirmation before overwriting the settled review", async () => {
    const onRecord = vi.fn();
    renderPane({ record_results: { "build-test": recorded() }, profile }, { onRecord });
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    await user.click(screen.getByRole("button", { name: /^re-record$/i }));
    expect(onRecord).not.toHaveBeenCalled();
    expect(screen.getByText(/replaced once the new recording settles/i)).toBeInTheDocument();
  });

  it("Re-record fires onRecord only after the confirm dialog is accepted", async () => {
    const onRecord = vi.fn();
    renderPane({ record_results: { "build-test": recorded() }, profile }, { onRecord });
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    await user.click(screen.getByRole("button", { name: /^re-record$/i }));
    const dialog = screen.getByRole("alertdialog");
    await user.click(within(dialog).getByRole("button", { name: /^re-record$/i }));
    expect(onRecord).toHaveBeenCalledWith("build & test");
  });
});
