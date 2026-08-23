/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";
import type { RecordResult, SetupStatus, Workspace } from "../../../lib/types";
import { OperatorProvider } from "../../wardyn/operator-context";

const getWorkspaceMock = vi.fn();
const deleteWorkspaceMock = vi.fn();
const setRequirementsMock = vi.fn();
const setWorkspaceLLMCredMock = vi.fn();
const recordTaskMock = vi.fn();
const promoteRecordEgressMock = vi.fn();
const setApprovedEgressMock = vi.fn();
const setDeniedEgressMock = vi.fn();
const buildWorkspaceMock = vi.fn();
vi.mock("../../../lib/api/workspaces", () => ({
  workspaces: {
    getWorkspace: (...a: unknown[]) => getWorkspaceMock(...a),
    deleteWorkspace: (...a: unknown[]) => deleteWorkspaceMock(...a),
    setRequirements: (...a: unknown[]) => setRequirementsMock(...a),
    setWorkspaceLLMCred: (...a: unknown[]) => setWorkspaceLLMCredMock(...a),
    recordTask: (...a: unknown[]) => recordTaskMock(...a),
    promoteRecordEgress: (...a: unknown[]) => promoteRecordEgressMock(...a),
    setApprovedEgress: (...a: unknown[]) => setApprovedEgressMock(...a),
    setDeniedEgress: (...a: unknown[]) => setDeniedEgressMock(...a),
    buildWorkspace: (...a: unknown[]) => buildWorkspaceMock(...a),
    createWorkspace: vi.fn(),
  },
}));
const getSetupStatusMock = vi.fn();
vi.mock("../../../lib/api/setup", () => ({
  setup: { getSetupStatus: (...a: unknown[]) => getSetupStatusMock(...a) },
}));
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
    providers: [],
    secrets: { present: [], github_app: false },
    age_key: { durable: false },
    has_runs: false,
    platform: { os: "linux", wsl: false, kvm: true },
    ...overrides,
  };
}

// Reads location.state so the CTA's contract with /runs (#10/D14: route
// state opens the New-Run dialog on arrival) is provable from THIS side
// without depending on the real RunsScreen or its own API surface.
function RunsRouteProbe() {
  const location = useLocation();
  const state = location.state as { openNewRun?: boolean } | null;
  return <div>runs screen{state?.openNewRun ? " (openNewRun)" : ""}</div>;
}

function renderDetail(id = "ws-1", operator = true) {
  return render(
    <MemoryRouter initialEntries={[`/workspaces/${id}`]}>
      <OperatorProvider operator={operator}>
        <Routes>
          <Route path="/workspaces/:id" element={<WorkspaceDetailScreen />} />
          <Route path="/workspaces" element={<div>back on the list</div>} />
          <Route path="/runs" element={<RunsRouteProbe />} />
          <Route path="/runs/:id" element={<div>run detail screen</div>} />
        </Routes>
      </OperatorProvider>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  getSetupStatusMock.mockResolvedValue(setupStatus());
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

describe("WorkspaceDetailScreen — header: name, source line, and Start a run", () => {
  it("shows the muted mono kind · source · ref line and offers Start a run unconditionally — no scan/status gating", async () => {
    getWorkspaceMock.mockResolvedValue(ws());
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderDetail();
    expect(await screen.findByRole("heading", { name: "payments" })).toBeInTheDocument();
    expect(screen.getByText("repo · acme/payments · main")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: /^start a run$/i }));
    expect(await screen.findByText("runs screen (openNewRun)")).toBeInTheDocument();
  });
});

describe("WorkspaceDetailScreen — image row: three honest variants, never a /build fetch to find out", () => {
  it("standard sandbox image: no base_image, nothing detected — no Rebuild action", async () => {
    getWorkspaceMock.mockResolvedValue(ws());
    renderDetail();
    expect(await screen.findByText("standard sandbox image")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Rebuild" })).not.toBeInTheDocument();
  });

  it("devcontainer.json: the scan detected one — Rebuild calls buildWorkspace", async () => {
    getWorkspaceMock.mockResolvedValue(ws({ profile: { has_devcontainer: true } as unknown as Record<string, unknown> }));
    buildWorkspaceMock.mockResolvedValue({ state: "building" });
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderDetail();
    expect(await screen.findByText(".devcontainer/devcontainer.json (this repo, @main)")).toBeInTheDocument();
    expect(screen.getByText("Built as written — we don't modify it.")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Rebuild" }));
    await waitFor(() => expect(buildWorkspaceMock).toHaveBeenCalledWith("ws-1"));
  });

  it("pinned ref: an explicit base_image — no Rebuild action, the ref renders mono", async () => {
    getWorkspaceMock.mockResolvedValue(ws({ base_image: { kind: "byo", image: "ghcr.io/acme/dev@sha256:ab12" } }));
    renderDetail();
    expect(await screen.findByText("ghcr.io/acme/dev@sha256:ab12")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Rebuild" })).not.toBeInTheDocument();
  });
});

describe("WorkspaceDetailScreen — Delete workspace", () => {
  it("deletes and navigates back to the list", async () => {
    getWorkspaceMock.mockResolvedValue(ws());
    deleteWorkspaceMock.mockResolvedValue(undefined);
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderDetail();
    await user.click(await screen.findByRole("button", { name: /delete this workspace/i }));
    await user.click(await screen.findByRole("button", { name: /^delete workspace$/i }));
    await waitFor(() => expect(deleteWorkspaceMock).toHaveBeenCalledWith("ws-1"));
    expect(await screen.findByText("back on the list")).toBeInTheDocument();
  });

  it("a viewer sees the trigger disabled", async () => {
    getWorkspaceMock.mockResolvedValue(ws());
    renderDetail("ws-1", false);
    expect(await screen.findByRole("button", { name: /delete this workspace/i })).toBeDisabled();
  });
});

describe("WorkspaceDetailScreen — three cards only", () => {
  it("renders 'Recorded sessions' (renamed from 'Sessions'), 'Allowed hosts', and 'Denied hosts', never the retired Requirements/Detected/Env-as-code cards", async () => {
    getWorkspaceMock.mockResolvedValue(ws());
    renderDetail();
    expect(await screen.findByText("Recorded sessions")).toBeInTheDocument();
    expect(screen.getByText(/Run a task once with everything open/)).toBeInTheDocument();
    expect(screen.getByText("Allowed hosts · 1")).toBeInTheDocument();
    expect(screen.getByText("Denied hosts · 0")).toBeInTheDocument();
    expect(screen.queryByText("Requirements")).not.toBeInTheDocument();
    expect(screen.queryByText("Detected, not required")).not.toBeInTheDocument();
    expect(screen.queryByText("Env as code")).not.toBeInTheDocument();
  });

  // ui-wsDetail-4: the header's source-path CopyButton must say what it
  // copies — a bare "Copy" accessible name doesn't tell a screen-reader user
  // what's on their clipboard.
  it("the source-path CopyButton's accessible name says what it copies", async () => {
    getWorkspaceMock.mockResolvedValue(ws());
    renderDetail();
    await screen.findByText("Recorded sessions");
    expect(screen.getByRole("button", { name: "Copy source path" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /^copy$/i })).not.toBeInTheDocument();
  });
});

describe("WorkspaceDetailScreen — Allowed hosts card", () => {
  it("a repo's clone host is structural: shown, no remove control", async () => {
    getWorkspaceMock.mockResolvedValue(ws());
    renderDetail();
    expect(await screen.findByText("Allowed hosts · 1")).toBeInTheDocument();
    expect(screen.getByText("github.com")).toBeInTheDocument();
    expect(screen.getByText("clone host for this workspace")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /remove github.com/i })).not.toBeInTheDocument();
  });

  it("an approved host renders a remove control that PUTs the narrowed allowlist", async () => {
    getWorkspaceMock.mockResolvedValue(ws({ approved_egress: ["registry.npmjs.org"] }));
    setApprovedEgressMock.mockResolvedValue(ws({ approved_egress: [] }));
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderDetail();
    expect(await screen.findByText("Allowed hosts · 2")).toBeInTheDocument();
    expect(screen.getByText("approved for this workspace")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Remove registry.npmjs.org" }));
    await waitFor(() => expect(setApprovedEgressMock).toHaveBeenCalledWith("ws-1", []));
  });

  it("a host promoted from a recorded session is attributed to it by name", async () => {
    const rr: RecordResult = {
      run_id: "r1",
      label: "build-and-test",
      mode: "interactive",
      status: "recorded",
      egress_promoted: true,
      observations: {
        domains: [{ host: "pypi.org", allow_count: 1, deny_count: 0, pending_count: 0 }],
        minted_grant_ids: [],
      } as unknown as RecordResult["observations"],
    };
    getWorkspaceMock.mockResolvedValue(ws({ approved_egress: ["pypi.org"], record_results: { "build-test": rr } }));
    renderDetail();
    expect(await screen.findByText('promoted from session "build-and-test"')).toBeInTheDocument();
  });
});

describe("WorkspaceDetailScreen — Denied hosts card", () => {
  it("shows nothing denied by default", async () => {
    getWorkspaceMock.mockResolvedValue(ws());
    renderDetail();
    expect(await screen.findByText("Denied hosts · 0")).toBeInTheDocument();
    expect(screen.getByText("Nothing denied.")).toBeInTheDocument();
  });

  it("a denied host renders a remove control that PUTs the narrowed denylist", async () => {
    getWorkspaceMock.mockResolvedValue(ws({ denied_egress: ["evil.example.com"] }));
    setDeniedEgressMock.mockResolvedValue(ws({ denied_egress: [] }));
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderDetail();
    expect(await screen.findByText("Denied hosts · 1")).toBeInTheDocument();
    expect(screen.getByText("denied for this workspace")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Remove evil.example.com" }));
    await waitFor(() => expect(setDeniedEgressMock).toHaveBeenCalledWith("ws-1", []));
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
    // A host approved EARLIER is in the workspace's allowlist but was not
    // observed by this recording. The retired 404-fallback computed
    // approved ∪ observed and PUT that whole union; the third argument is now
    // the promote SUBSET the confirm approved, so this host must not appear.
    getWorkspaceMock.mockResolvedValue(
      ws({ record_results: { "build-test": rr }, approved_egress: ["already.example.com"] }),
    );
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

  // UI-WS-14: requestPromoteEgress used to subtract only ws.approved_egress —
  // session-helpers.ts's newEgressHosts (which drives the button's own
  // count) ALSO subtracts profile.egress_domains, so the confirm could list
  // an auto-allowed host the button never asked about. The bulk list is no
  // longer read-only either: it checkboxes, and only the CHECKED subset is
  // posted (the server's own optional {"hosts":[…]} field).
  it("the confirm lists exactly what the button counted, and promotes only the hosts left checked", async () => {
    const rr: RecordResult = {
      run_id: "r1",
      label: "build & test",
      mode: "interactive",
      status: "recorded",
      observations: {
        domains: [
          { host: "proxy.golang.org", allow_count: 1, deny_count: 0, pending_count: 0 },
          { host: "nexus.corp.internal", allow_count: 1, deny_count: 0, pending_count: 0 },
          { host: "files.pythonhosted.org", allow_count: 1, deny_count: 0, pending_count: 0 },
        ],
        minted_grant_ids: [],
      } as unknown as RecordResult["observations"],
    };
    getWorkspaceMock.mockResolvedValue(
      ws({
        record_results: { "build-test": rr },
        profile: { egress_domains: ["proxy.golang.org"] } as unknown as Record<string, unknown>,
      }),
    );
    promoteRecordEgressMock.mockResolvedValue(ws({}));
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderDetail();

    // The button already excludes the auto-allowed host from its own count.
    await user.click(await screen.findByRole("button", { name: /approve 2 observed hosts/i }));
    const dialog = within(await screen.findByRole("alertdialog"));
    expect(dialog.getByText(/nexus\.corp\.internal/)).toBeInTheDocument();
    expect(dialog.queryByText(/proxy\.golang\.org/)).not.toBeInTheDocument();

    // Everything starts checked; unchecking one must drop it from the POST.
    await user.click(dialog.getByRole("checkbox", { name: /approve files\.pythonhosted\.org/i }));
    await user.click(dialog.getByRole("button", { name: /approve 1 host$/i }));
    await waitFor(() =>
      expect(promoteRecordEgressMock).toHaveBeenCalledWith("ws-1", "build-test", ["nexus.corp.internal"]),
    );
  });
});

// This screen never read useOperator at all — a viewer saw enabled
// Delete/lane toggles/record controls that all 403 server-side.
describe("WorkspaceDetailScreen — a viewer's write controls are disabled", () => {
  it("disables the Sessions card's session controls, the Allowed hosts remove control, and the Denied hosts remove control", async () => {
    getWorkspaceMock.mockResolvedValue(
      ws({
        approved_egress: ["registry.npmjs.org"],
        denied_egress: ["evil.example.com"],
        record_results: {
          "build-test": { run_id: "r1", label: "build & test", mode: "interactive", status: "recorded" },
        },
      }),
    );
    renderDetail("ws-1", false);

    await screen.findByText("Recorded sessions");
    // Unconfounded by any OTHER disabled condition (busyTask, empty name):
    // proves the operator gate itself, not something else that happens to
    // already disable the control.
    expect(screen.getByLabelText(/session name/i)).toBeDisabled();
    expect(screen.getByRole("button", { name: /re-record/i })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Remove registry.npmjs.org" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Remove evil.example.com" })).toBeDisabled();
  });
});
