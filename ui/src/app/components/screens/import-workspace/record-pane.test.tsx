/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ProfileObservations, RecordResult, Workspace } from "../../../lib/types";

// The embedded terminal is heavy (xterm) and irrelevant to what this pane decides,
// so stub it to a marker — we only assert it MOUNTS for a recording session.
vi.mock("../../attach-terminal", () => ({
  AttachTerminal: ({ runId }: { runId: string }) => <div data-testid="attach-terminal">{runId}</div>,
}));

// LiveApprovals (verify sessions) polls the approvals API; mock it so the pane's
// live-approval widget is testable without a server.
const listApprovalsMock = vi.fn((..._a: unknown[]): Promise<unknown[]> => Promise.resolve([]));
const approveMock = vi.fn();
const denyMock = vi.fn();
vi.mock("../../../lib/api", () => ({
  api: {
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
  handlers: Partial<Record<string, ReturnType<typeof vi.fn>>> = {},
  composerEnabled = true,
  confined = false,
) {
  return render(
    <RecordPane
      ws={ws(over)}
      confined={confined}
      notice={null}
      busyTask={null}
      composerEnabled={composerEnabled}
      onRecord={handlers.onRecord ?? noop}
      onDoneRecording={handlers.onDoneRecording ?? noop}
      onPromoteEgress={handlers.onPromoteEgress ?? noop}
      onApproveHost={handlers.onApproveHost ?? noop}
      onOpenProfile={handlers.onOpenProfile ?? noop}
    />,
  );
}

beforeEach(() => {
  localStorage.clear(); // getDefaultCc() => null => tier CC1 => banner shown
});

describe("RecordPane — header, CC1 banner, model note", () => {
  it("shows the recommended/skippable chip and the honest CC1 open-egress banner", () => {
    renderPane();
    expect(screen.getByText(/recommended/i)).toBeInTheDocument();
    const banner = screen.getByTestId("record-cc1-banner");
    expect(banner).toHaveTextContent(/fence/i);
    expect(banner).toHaveTextContent(/egress/i);
  });

  it("hides the CC1 banner when the operator's default tier is stronger", () => {
    localStorage.setItem("wardyn-default-confinement", "CC3");
    renderPane();
    expect(screen.queryByTestId("record-cc1-banner")).not.toBeInTheDocument();
  });

  it("notes the configured model provider when composer is enabled", () => {
    renderPane({}, {}, true);
    expect(screen.getByText(/configured model provider/i)).toBeInTheDocument();
  });

  it("warns when no model provider is configured", () => {
    renderPane({}, {}, false);
    expect(screen.getByText(/no model provider is configured/i)).toBeInTheDocument();
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
});

describe("RecordPane — session lifecycle", () => {
  it("a recording session embeds the attach terminal, shows detected-command hints, and Done", async () => {
    const onDoneRecording = vi.fn();
    const rr: RecordResult = { run_id: "run-42", label: "build & test", mode: "interactive", status: "recording" };
    renderPane(
      {
        record_results: { "build-test": rr },
        profile: { setup_commands: [{ stage: "install", command: "npm ci" }] } as unknown as Workspace["profile"],
      },
      { onDoneRecording },
    );
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    expect(screen.getByTestId("attach-terminal")).toHaveTextContent("run-42");
    expect(screen.getByText("npm ci")).toBeInTheDocument(); // detected-command hint pill
    await user.click(screen.getByRole("button", { name: /done recording/i }));
    expect(onDoneRecording).toHaveBeenCalledWith("run-42");
  });
});

describe("RecordPane — settled review card", () => {
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

  it("shows a Promoted badge (no approve button) once egress is promoted", () => {
    renderPane({ record_results: { "build-test": recorded({ egress_promoted: true }) }, profile });
    const review = screen.getByTestId("record-review");
    expect(within(review).getByText(/promoted/i)).toBeInTheDocument();
    expect(within(review).queryByRole("button", { name: /approve .* observed host/i })).not.toBeInTheDocument();
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
    // Now also passes a suggested "save as is" name = workspace-recording.
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
});

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

describe("RecordPane — verify mode (replay a recording confined)", () => {
  // The recording made on the Record step (open). Verify lists THESE, not a name box.
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

  it("lists the operator's recordings (not a name box) with a Verify action each", async () => {
    const onRecord = vi.fn();
    renderPane({ record_results: { "build-test": learning } }, { onRecord }, true, true);
    // No free-text session naming on Verify.
    expect(screen.queryByTestId("record-new-session")).not.toBeInTheDocument();
    // The recording is listed and replayable.
    const card = screen.getByTestId("verify-recording-build-test");
    expect(within(card).getByText("build & test")).toBeInTheDocument();
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    await user.click(within(card).getByRole("button", { name: /verify this recording/i }));
    expect(onRecord).toHaveBeenCalledWith("build & test"); // panel routes it confined
  });

  it("shows the containment review once a recording's verify replay settles", async () => {
    const onApproveHost = vi.fn();
    renderPane(
      {
        record_results: { "build-test": learning, "verify:build-test": confinedRR },
        approved_egress: ["github.com"],
      },
      { onApproveHost },
      true,
      true,
    );
    const blocked = screen.getByTestId("verify-session-blocked");
    expect(within(blocked).getByText("evil.example.com")).toBeInTheDocument();
    // github.com is already approved → never shown as blocked/off-policy.
    expect(within(blocked).queryByText("github.com")).not.toBeInTheDocument();
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    await user.click(within(blocked).getByRole("button", { name: /approve/i }));
    expect(onApproveHost).toHaveBeenCalledWith("evil.example.com");
  });

  it("embeds the attach terminal while a verify replay is recording", () => {
    const running: RecordResult = { run_id: "vr9", label: "build & test", mode: "interactive", confined: true, status: "recording" };
    renderPane({ record_results: { "build-test": learning, "verify:build-test": running } }, {}, true, true);
    expect(screen.getByTestId("attach-terminal")).toHaveTextContent("vr9");
  });

  it("shows an empty state (record first) when there are no recordings to verify", () => {
    renderPane({}, {}, true, true);
    expect(screen.getByTestId("verify-no-recordings")).toBeInTheDocument();
  });

  it("surfaces the run's pending off-policy approval live and denies it inline", async () => {
    listApprovalsMock.mockReset();
    denyMock.mockReset();
    listApprovalsMock.mockResolvedValue([
      { id: "apr1", run_id: "vr9", kind: "egress_domain", requested_scope: { host: "evil.example.com" }, state: "PENDING", requested_at: "" },
      { id: "aprX", run_id: "other", kind: "egress_domain", requested_scope: { host: "other.com" }, state: "PENDING", requested_at: "" },
    ]);
    const running: RecordResult = { run_id: "vr9", label: "build & test", mode: "interactive", confined: true, status: "recording" };
    renderPane({ record_results: { "build-test": learning, "verify:build-test": running } }, {}, true, true);

    // Only THIS run's pending approval shows (the other run's is filtered out).
    const panel = await screen.findByTestId("live-approvals");
    expect(within(panel).getByText("evil.example.com")).toBeInTheDocument();
    expect(within(panel).queryByText("other.com")).not.toBeInTheDocument();

    const user = userEvent.setup({ pointerEventsCheck: 0 });
    await user.click(within(panel).getByRole("button", { name: /deny/i }));
    expect(denyMock).toHaveBeenCalledWith("apr1", expect.any(String));
  });
});
