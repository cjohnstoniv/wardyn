/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { toast } from "sonner";
import type { SetupStatus, Workspace, WorkspaceProfile } from "../../../lib/types";

// The Import panel is driven entirely off api + the workspace status, so the api
// is mocked and each test hands in a workspace at a known status to assert the
// rail resumes on the right step and each pane renders the right actions.
const getWorkspaceMock = vi.fn();
const listWorkspacesMock = vi.fn();
const listSecretsMock = vi.fn();
const scanWorkspaceMock = vi.fn();
const recordTaskMock = vi.fn();
const getObservedEgressMock = vi.fn();
const promoteRecordEgressMock = vi.fn();
const getSetupStatusMock = vi.fn();

vi.mock("../../../lib/api/workspaces", () => ({
  workspaces: {
    getWorkspace: (...a: unknown[]) => getWorkspaceMock(...a),
    listWorkspaces: (...a: unknown[]) => listWorkspacesMock(...a),
    scanWorkspace: (...a: unknown[]) => scanWorkspaceMock(...a),
    recordTask: (...a: unknown[]) => recordTaskMock(...a),
    getObservedEgress: (...a: unknown[]) => getObservedEgressMock(...a),
    promoteRecordEgress: (...a: unknown[]) => promoteRecordEgressMock(...a),
    // Referenced by the embedded Add* dialogs only when opened; present so a
    // stray effect never throws "undefined is not a function".
    createWorkspace: vi.fn(),
    updateWorkspace: vi.fn(),
  },
}));
vi.mock("../../../lib/api/secrets", () => ({
  secrets: { listSecrets: (...a: unknown[]) => listSecretsMock(...a), setSecret: vi.fn() },
}));
vi.mock("../../../lib/api/setup", () => ({
  setup: { getSetupStatus: (...a: unknown[]) => getSetupStatusMock(...a) },
}));
vi.mock("../../../lib/api/runs", () => ({
  runs: { killRun: vi.fn() },
}));
vi.mock("sonner", () => ({
  toast: { error: vi.fn(), success: vi.fn(), info: vi.fn() },
}));

import { ImportWorkspaceDialog } from "./import-panel";

function ws(over: Partial<Workspace> = {}, profile: WorkspaceProfile = {}): Workspace {
  return {
    id: "ws-1",
    name: "payments",
    kind: "repo",
    source: "acme/payments",
    status: "scanned",
    created_at: "",
    updated_at: "",
    profile: profile as unknown as Record<string, unknown>,
    ...over,
  };
}

// Minimal SetupStatus fixture (mirrors setup-screen.test.tsx's baseStatus) — no
// LLM path by default, so RecordPane's model-readiness warning renders unless a
// test opts a provider/secret in via overrides.
function setupStatus(overrides: Partial<SetupStatus> = {}): SetupStatus {
  return {
    ready: false,
    checks: [],
    auth: { mode: "local", local_loopback: true },
    runner: { driver: "docker", confinement_classes: ["CC1", "CC2"] },
    composer: { enabled: false, backends: [] },
    providers: [{ tool: "claude", installed: true, logged_in: false }],
    secrets: { present: [], github_app: false },
    age_key: { durable: false },
    has_runs: false,
    platform: { os: "linux", wsl: false, kvm: true },
    ...overrides,
  };
}

beforeEach(() => {
  vi.clearAllMocks();
  listWorkspacesMock.mockResolvedValue([]);
  listSecretsMock.mockResolvedValue([]);
  getObservedEgressMock.mockResolvedValue({ denied: [], runs_examined: 0 });
  // no LLM path by default (see setupStatus()); the suite below opts a
  // provider in to prove the warning is driven by /setup/status.
  getSetupStatusMock.mockResolvedValue(setupStatus());
});

describe("ImportWorkspaceDialog — rail resumes on the workspace's status", () => {
  it("renders all four rail steps and resumes a scanned workspace on Configure", async () => {
    getWorkspaceMock.mockResolvedValue(
      ws({ status: "scanned" }, { required_secrets: [{ name: "DATABASE_URL", kind: "postgres" }] }),
    );
    render(<ImportWorkspaceDialog open workspaceId="ws-1" onOpenChange={() => {}} onReload={() => {}} />);

    // The rail shows every surviving step — Verify/Finalize are retired.
    for (const label of ["Source", "Scan", "Configure", "Record"]) {
      expect(await screen.findByText(label)).toBeInTheDocument();
    }
    // Configure pane content: security chip + the declared secret (setup
    // commands are no longer approved/edited here).
    expect(await screen.findByText(/secrets are brokered/i)).toBeInTheDocument();
    expect(screen.getByText("DATABASE_URL")).toBeInTheDocument();
  });

  // Legacy: a row stamped by the retired automated build/verify can still
  // carry this status until the migration wave (activeStepForStatus's own
  // unit tests in import-types.test.ts pin the full legacy mapping).
  it("resumes a legacy verify_failed workspace on Record, not a dead rail position", async () => {
    getWorkspaceMock.mockResolvedValue(ws({ status: "verify_failed" }));
    render(<ImportWorkspaceDialog open workspaceId="ws-1" onOpenChange={() => {}} onReload={() => {}} />);

    expect(await screen.findByText(/record sessions/i)).toBeInTheDocument();
  });
});

describe("ImportWorkspaceDialog — Done replaces the old Verify/Finalize steps", () => {
  it("Configure's Done finishes without ever visiting Record", async () => {
    getWorkspaceMock.mockResolvedValue(ws({ status: "scanned" }));
    const onOpenChange = vi.fn();
    const onReload = vi.fn();
    render(<ImportWorkspaceDialog open workspaceId="ws-1" onOpenChange={onOpenChange} onReload={onReload} />);
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    await screen.findByText(/secrets are brokered/i);
    await user.click(screen.getByRole("button", { name: /^done$/i }));

    expect(toast.success).toHaveBeenCalledWith("Workspace added");
    expect(onReload).toHaveBeenCalledTimes(1);
    expect(onOpenChange).toHaveBeenCalledWith(false);
  });

  it("Record's Done closes and toasts 'Workspace added', with no server round-trip", async () => {
    getWorkspaceMock.mockResolvedValue(ws({ status: "scanned" }));
    const onOpenChange = vi.fn();
    const onReload = vi.fn();
    render(<ImportWorkspaceDialog open workspaceId="ws-1" onOpenChange={onOpenChange} onReload={onReload} />);
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    await user.click(await screen.findByRole("button", { name: /next: record/i }));
    await screen.findByText(/record sessions/i);
    await user.click(screen.getByRole("button", { name: /^done$/i }));

    expect(toast.success).toHaveBeenCalledWith("Workspace added");
    expect(onReload).toHaveBeenCalledTimes(1);
    expect(onOpenChange).toHaveBeenCalledWith(false);
  });
});

// The Record pane's one-click (approveHost) and bulk (promoteEgress) egress
// approvals used to PUT straight to the api — skipping the same untrusted-
// content confirm the Workspaces screen enforces for the identical action (the
// host names come from a run's observed egress, not something the operator
// typed). Record's bulk "Approve N observed hosts" is the surviving trigger
// now that the confined Verify flow (the original regression's trigger) is
// retired from this interim panel.
describe("ImportWorkspaceDialog — egress approvals confirm before applying", () => {
  const recordedWithNewHost = () =>
    ws({
      status: "scanned",
      record_results: {
        "build-test": {
          run_id: "r1",
          label: "build & test",
          mode: "interactive",
          status: "recorded",
          observations: {
            domains: [{ host: "evil.example.com", allow_count: 1, deny_count: 0, pending_count: 0 }],
            minted_grant_ids: [],
            exec_argv0s: [],
            file_writes: [],
            connects: [],
            anomalies: [],
          },
        },
      },
    });

  it("does not call promoteRecordEgress until the confirm dialog is accepted", async () => {
    getWorkspaceMock.mockResolvedValue(recordedWithNewHost());
    promoteRecordEgressMock.mockResolvedValue(recordedWithNewHost());
    render(<ImportWorkspaceDialog open workspaceId="ws-1" onOpenChange={() => {}} onReload={() => {}} />);
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    await user.click(await screen.findByRole("button", { name: /next: record/i }));
    await user.click(await screen.findByRole("button", { name: /approve 1 observed host/i }));

    // The confirm dialog names the host and blocks the write until accepted.
    expect(await screen.findByText(/approve egress to evil\.example\.com/i)).toBeInTheDocument();
    expect(promoteRecordEgressMock).not.toHaveBeenCalled();

    await user.click(screen.getByRole("button", { name: /approve host/i }));
    await waitFor(() =>
      expect(promoteRecordEgressMock).toHaveBeenCalledWith("ws-1", "build-test", ["evil.example.com"]),
    );
  });

  it("cancelling the confirm dialog leaves the host unapproved", async () => {
    getWorkspaceMock.mockResolvedValue(recordedWithNewHost());
    render(<ImportWorkspaceDialog open workspaceId="ws-1" onOpenChange={() => {}} onReload={() => {}} />);
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    await user.click(await screen.findByRole("button", { name: /next: record/i }));
    await user.click(await screen.findByRole("button", { name: /approve 1 observed host/i }));
    await screen.findByText(/approve egress to evil\.example\.com/i);

    await user.click(screen.getByRole("button", { name: /^cancel$/i }));
    expect(promoteRecordEgressMock).not.toHaveBeenCalled();
  });
});

// Record's "no model configured" warning must be composer-INDEPENDENT —
// wiring it to composer detection fired the warning even with a connected
// subscription or API key. It reflects GET /setup/status (hasLlmPath) instead.
describe("ImportWorkspaceDialog — Record model-readiness reflects /setup/status, not composer", () => {
  it("shows the connected note when a provider is logged in, even with no composer backend", async () => {
    getWorkspaceMock.mockResolvedValue(ws({ status: "scanned" }));
    getSetupStatusMock.mockResolvedValue(
      setupStatus({
        providers: [{ tool: "claude", installed: true, logged_in: true, auth_mode: "subscription" }],
      }),
    );
    render(<ImportWorkspaceDialog open workspaceId="ws-1" onOpenChange={() => {}} onReload={() => {}} />);
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    await user.click(await screen.findByRole("button", { name: /next: record/i }));
    expect(await screen.findByText(/configured model provider/i)).toBeInTheDocument();
    expect(screen.queryByText(/no model provider is configured/i)).not.toBeInTheDocument();
  });

  it("still warns when /setup/status reports no provider, key, or composer backend", async () => {
    getWorkspaceMock.mockResolvedValue(ws({ status: "scanned" }));
    // default setupStatus() (from beforeEach) has no logged-in provider/secret.
    render(<ImportWorkspaceDialog open workspaceId="ws-1" onOpenChange={() => {}} onReload={() => {}} />);
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    await user.click(await screen.findByRole("button", { name: /next: record/i }));
    expect(await screen.findByText(/no model provider is configured/i)).toBeInTheDocument();
  });
});
