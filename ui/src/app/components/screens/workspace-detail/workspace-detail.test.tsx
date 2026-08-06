/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import type { RecordResult, SetupStatus, Workspace } from "../../../lib/types";

const getWorkspaceMock = vi.fn();
const scanWorkspaceMock = vi.fn();
const deleteWorkspaceMock = vi.fn();
const setRequirementsMock = vi.fn();
const getObservedEgressMock = vi.fn();
const setWorkspaceLLMCredMock = vi.fn();
const recordTaskMock = vi.fn();
const promoteRecordEgressMock = vi.fn();
const setApprovedEgressMock = vi.fn();
const getEnvAsCodeMock = vi.fn();
const writeEnvAsCodeMock = vi.fn();
const updateWorkspaceMock = vi.fn();
vi.mock("../../../lib/api/workspaces", () => ({
  workspaces: {
    getWorkspace: (...a: unknown[]) => getWorkspaceMock(...a),
    scanWorkspace: (...a: unknown[]) => scanWorkspaceMock(...a),
    deleteWorkspace: (...a: unknown[]) => deleteWorkspaceMock(...a),
    setRequirements: (...a: unknown[]) => setRequirementsMock(...a),
    getObservedEgress: (...a: unknown[]) => getObservedEgressMock(...a),
    setWorkspaceLLMCred: (...a: unknown[]) => setWorkspaceLLMCredMock(...a),
    recordTask: (...a: unknown[]) => recordTaskMock(...a),
    promoteRecordEgress: (...a: unknown[]) => promoteRecordEgressMock(...a),
    setApprovedEgress: (...a: unknown[]) => setApprovedEgressMock(...a),
    getEnvAsCode: (...a: unknown[]) => getEnvAsCodeMock(...a),
    writeEnvAsCode: (...a: unknown[]) => writeEnvAsCodeMock(...a),
    updateWorkspace: (...a: unknown[]) => updateWorkspaceMock(...a),
    createWorkspace: vi.fn(),
  },
}));
const listSecretsMock = vi.fn();
vi.mock("../../../lib/api/secrets", () => ({
  secrets: { listSecrets: (...a: unknown[]) => listSecretsMock(...a) },
}));
const getSetupStatusMock = vi.fn();
vi.mock("../../../lib/api/setup", () => ({
  setup: { getSetupStatus: (...a: unknown[]) => getSetupStatusMock(...a) },
}));
// The "Edit workspace…" kebab item now mounts the real WorkspaceWizard (see
// wizard.test.tsx's identical mock — its mount effect fetches this too).
const listIntegrationsMock = vi.fn();
vi.mock("../../../lib/api/integrations", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/integrations")>(
    "../../../lib/api/integrations",
  );
  return { ...actual, integrationsApi: { list: () => listIntegrationsMock() } };
});
const killRunMock = vi.fn();
vi.mock("../../../lib/api/runs", () => ({ runs: { killRun: (...a: unknown[]) => killRunMock(...a) } }));
vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn(), info: vi.fn() } }));

import { WorkspaceDetailScreen } from "./workspace-detail";

function ws(over: Partial<Workspace> = {}): Workspace {
  return {
    id: "ws-1",
    name: "payments",
    kind: "repo",
    source: "acme/payments",
    ref: "main",
    status: "scanned",
    created_at: "",
    updated_at: "",
    ...over,
  };
}

function setupStatus(overrides: Partial<SetupStatus> = {}): SetupStatus {
  return {
    ready: false,
    checks: [],
    auth: { mode: "local", local_loopback: true },
    runner: { driver: "docker", confinement_classes: ["CC1"] },
    composer: { enabled: false, backends: [] },
    providers: [],
    secrets: { present: [], github_app: false },
    age_key: { durable: false },
    has_runs: false,
    platform: { os: "linux", wsl: false, kvm: true },
    ...overrides,
  };
}

function renderDetail(id = "ws-1") {
  return render(
    <MemoryRouter initialEntries={[`/workspaces/${id}`]}>
      <Routes>
        <Route path="/workspaces/:id" element={<WorkspaceDetailScreen />} />
        <Route path="/workspaces" element={<div>back on the list</div>} />
        <Route path="/runs" element={<div>runs screen</div>} />
        <Route path="/runs/:id" element={<div>run detail screen</div>} />
      </Routes>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  listSecretsMock.mockResolvedValue([]);
  getSetupStatusMock.mockResolvedValue(setupStatus());
  getObservedEgressMock.mockResolvedValue({ denied: [], runs_examined: 0 });
  listIntegrationsMock.mockResolvedValue({ ai: [], scm: [] });
});

describe("WorkspaceDetailScreen — not found", () => {
  it("shows a not-found state and navigates back to the list", async () => {
    getWorkspaceMock.mockResolvedValue(undefined);
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderDetail();
    expect(await screen.findByText(/workspace not found/i)).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: /back to workspaces/i }));
    expect(await screen.findByText("back on the list")).toBeInTheDocument();
  });
});

describe("WorkspaceDetailScreen — header: status chip + story sentence + one primary action per state", () => {
  it("pending_scan: offers Scan now, which calls scanWorkspace", async () => {
    getWorkspaceMock.mockResolvedValue(ws({ status: "pending_scan" }));
    scanWorkspaceMock.mockResolvedValue({ async: false });
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderDetail();
    expect(await screen.findByText("Setting up")).toBeInTheDocument();
    expect(screen.getByText(/not scanned yet/i)).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: /^scan now$/i }));
    await waitFor(() => expect(scanWorkspaceMock).toHaveBeenCalledWith("ws-1"));
  });

  it("scanning with an active_run_id: Watch the run is enabled and navigates to the run", async () => {
    getWorkspaceMock.mockResolvedValue(ws({ status: "scanning", active_run_id: "run-9" }));
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderDetail();
    const watch = await screen.findByRole("button", { name: /watch the run/i });
    expect(watch).toBeEnabled();
    await user.click(watch);
    expect(await screen.findByText("run detail screen")).toBeInTheDocument();
  });

  it("scanning with no active_run_id yet: Watch the run is disabled", async () => {
    getWorkspaceMock.mockResolvedValue(ws({ status: "scanning" }));
    renderDetail();
    expect(await screen.findByRole("button", { name: /watch the run/i })).toBeDisabled();
  });

  it("error: offers Retry scan", async () => {
    getWorkspaceMock.mockResolvedValue(ws({ status: "error" }));
    scanWorkspaceMock.mockResolvedValue({ async: false });
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderDetail();
    expect(await screen.findByText("Scan failed")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: /retry scan/i }));
    await waitFor(() => expect(scanWorkspaceMock).toHaveBeenCalledWith("ws-1"));
  });

  it("usable: offers Start a run with this workspace", async () => {
    getWorkspaceMock.mockResolvedValue(ws({ status: "scanned" }));
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderDetail();
    expect(await screen.findByText("Usable")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: /start a run with this workspace/i }));
    expect(await screen.findByText("runs screen")).toBeInTheDocument();
  });
});

describe("WorkspaceDetailScreen — kebab: Edit workspace… / Rescan… (destructive confirm) / Delete…", () => {
  it("Edit workspace… opens the wizard hydrated on this row", async () => {
    getWorkspaceMock.mockResolvedValue(ws());
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderDetail();
    await user.click(await screen.findByRole("button", { name: /workspace actions/i }));
    await user.click(screen.getByRole("menuitem", { name: /edit workspace/i }));
    expect(await screen.findByRole("heading", { name: "Edit workspace" })).toBeInTheDocument();
  });

  it("Rescan… states what it destroys before scanning", async () => {
    getWorkspaceMock.mockResolvedValue(ws());
    scanWorkspaceMock.mockResolvedValue({ async: false });
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderDetail();
    await user.click(await screen.findByRole("button", { name: /workspace actions/i }));
    await user.click(screen.getByRole("menuitem", { name: /rescan/i }));
    expect(await screen.findByText(/requirements and recorded sessions are cleared/i)).toBeInTheDocument();
    expect(scanWorkspaceMock).not.toHaveBeenCalled();
    await user.click(screen.getByRole("button", { name: /^rescan$/i }));
    await waitFor(() => expect(scanWorkspaceMock).toHaveBeenCalledWith("ws-1"));
  });

  it("Delete… deletes and navigates back to the list", async () => {
    getWorkspaceMock.mockResolvedValue(ws());
    deleteWorkspaceMock.mockResolvedValue(undefined);
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderDetail();
    await user.click(await screen.findByRole("button", { name: /workspace actions/i }));
    await user.click(screen.getByRole("menuitem", { name: /delete/i }));
    await user.click(await screen.findByRole("button", { name: /delete workspace/i }));
    await waitFor(() => expect(deleteWorkspaceMock).toHaveBeenCalledWith("ws-1"));
    expect(await screen.findByText("back on the list")).toBeInTheDocument();
  });
});

describe("WorkspaceDetailScreen — all four cards for a non-container workspace", () => {
  it("renders Requirements, Detected, Sessions, and Env as code", async () => {
    getWorkspaceMock.mockResolvedValue(ws());
    renderDetail();
    expect(await screen.findByText("Requirements")).toBeInTheDocument();
    expect(screen.getByText("Detected, not required")).toBeInTheDocument();
    expect(screen.getByText("Sessions")).toBeInTheDocument();
    expect(screen.getByText("Env as code")).toBeInTheDocument();
  });
});

// Ported from the retired import-panel.test.tsx's "egress approvals confirm
// before applying" coverage — a session's one-click "Approve N observed
// hosts" is untrusted-content-derived (a run's own observed traffic, not
// something the operator typed), so it must gate behind the same confirm the
// rest of the app uses before actually promoting it.
describe("WorkspaceDetailScreen — a session's egress promotion confirms before applying", () => {
  it("does not call promoteRecordEgress until the confirm dialog is accepted", async () => {
    const rr: RecordResult = {
      run_id: "r1",
      label: "build & test",
      mode: "interactive",
      status: "recorded",
      observations: {
        domains: [{ host: "evil.example.com", allow_count: 1, deny_count: 0, pending_count: 0 }],
        minted_grant_ids: [],
      } as unknown as RecordResult["observations"],
    };
    getWorkspaceMock.mockResolvedValue(ws({ record_results: { "build-test": rr } }));
    promoteRecordEgressMock.mockResolvedValue(ws({}));
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderDetail();

    await user.click(await screen.findByRole("button", { name: /approve 1 observed host/i }));
    expect(await screen.findByText(/approve egress to evil\.example\.com/i)).toBeInTheDocument();
    expect(promoteRecordEgressMock).not.toHaveBeenCalled();

    await user.click(screen.getByRole("button", { name: /approve host/i }));
    await waitFor(() =>
      expect(promoteRecordEgressMock).toHaveBeenCalledWith("ws-1", "build-test", ["evil.example.com"]),
    );
  });
});
