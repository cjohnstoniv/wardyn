/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import type { SetupModelProvider, SetupStatus, Workspace } from "../../../lib/types";
import { OperatorProvider } from "../../wardyn/operator-context";
import { ModelAccessProvider } from "../../wardyn/model-access-context";
import { OPERATOR_ONLY_REASON } from "../../wardyn/copy";

const getWorkspaceMock = vi.fn();
const setWorkspaceLLMCredMock = vi.fn();
vi.mock("../../../lib/api/workspaces", () => ({
  workspaces: {
    getWorkspace: (...a: unknown[]) => getWorkspaceMock(...a),
    setWorkspaceLLMCred: (...a: unknown[]) => setWorkspaceLLMCredMock(...a),
  },
}));
vi.mock("../../../lib/api/setup", () => ({ setup: { getSetupStatus: () => Promise.resolve(setupStatus()) } }));
const getModelProvidersMock = vi.fn();
vi.mock("../../../lib/api/model-providers", () => ({
  modelProviders: { getModelProviders: (...a: unknown[]) => getModelProvidersMock(...a) },
}));
vi.mock("../../../lib/api/runs", () => ({ runs: { killRun: vi.fn() } }));
vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn(), info: vi.fn() } }));
vi.mock("../../../lib/api/permissions", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/permissions")>("../../../lib/api/permissions");
  return {
    ...actual,
    permissions: { ...actual.permissions, getAvailability: () => new Promise(() => {}), getMyCapabilities: () => new Promise(() => {}) },
  };
});

import { DENIED } from "../../../lib/permissions-copy";
import { WorkspaceDetailScreen } from "./workspace-detail";

function ws(over: Partial<Workspace> = {}): Workspace {
  return { id: "ws-1", name: "payments", kind: "repo", source: "acme/payments", ref: "main", status: "scanned", created_at: "", updated_at: "", ...over };
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

function renderDetail(operator = true, modelProviders?: SetupModelProvider[], principal?: string) {
  const screen = (
    <MemoryRouter initialEntries={["/workspaces/ws-1"]}>
      <OperatorProvider operator={operator} securityOperator={operator} principal={principal}>
        <Routes>
          <Route path="/workspaces/:id" element={<WorkspaceDetailScreen />} />
        </Routes>
      </OperatorProvider>
    </MemoryRouter>
  );
  if (modelProviders === undefined) return render(screen);
  return render(
    <ModelAccessProvider status={setupStatus({ model_providers: modelProviders })} onRefresh={() => {}}>
      {screen}
    </ModelAccessProvider>,
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  getModelProvidersMock.mockResolvedValue({ providers: { providers: [{ id: "mp-1", name: "Team gateway", kind: "openai_compatible" }] } });
});

// #2588: the header card's Model provider row, under Image. Change opens the existing Model access dialog for an
// admin (useOperator, not canMutate); a member who owns the workspace gets it disabled with the admin-role reason.
describe("WorkspaceDetailScreen — the Model provider row (#2588)", () => {
  const DESCRIPTION = "A run that picks this workspace/container uses this model provider unless it chooses one itself.";
  const changeButton = () => screen.getByRole("button", { name: "Change" });

  it("an admin sees the bound provider's id and Change opens the dialog; Save writes the new binding", async () => {
    getWorkspaceMock.mockResolvedValue(ws({ llm_cred: { provider_ref: "mp-gone" } }));
    setWorkspaceLLMCredMock.mockResolvedValue(ws({ llm_cred: { provider_ref: "mp-1" } }));
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    // The row names a provider by the display name the caller's own list carries.
    renderDetail(true, [{ id: "mp-1", name: "Team gateway", kind: "openai_compatible", harnesses: [], host: "" }]);

    expect(await screen.findByText("mp-gone")).toBeInTheDocument();
    expect(screen.getByText(DESCRIPTION)).toBeInTheDocument();
    expect(changeButton()).toBeEnabled();
    await user.click(changeButton());

    const dialog = await screen.findByRole("dialog", { name: "Model access — payments" });
    // The dead binding stays selectable so it can be cleared; picking a listed provider rebinds.
    expect(await within(dialog).findByText(/mp-gone \(not in the Model providers list\)/)).toBeInTheDocument();
    await user.click(await within(dialog).findByRole("radio", { name: /Team gateway/ }));
    await user.click(within(dialog).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(setWorkspaceLLMCredMock).toHaveBeenCalledWith("ws-1", { provider_ref: "mp-1" }));
    // The row now names the provider by its display name, and the dialog is gone.
    expect(await screen.findByText("Team gateway")).toBeInTheDocument();
    expect(screen.queryByRole("dialog", { name: "Model access — payments" })).not.toBeInTheDocument();
  });

  it("an unbound workspace reads None", async () => {
    getWorkspaceMock.mockResolvedValue(ws());
    renderDetail();
    expect(await screen.findByText("None")).toBeInTheDocument();
  });

  it("not an admin: Change is disabled with the admin-role reason, even for the workspace's owner", async () => {
    getWorkspaceMock.mockResolvedValue(ws({ owned_by: "dana@corp.example", llm_cred: { provider_ref: "mp-1" } }));
    render(
      <MemoryRouter initialEntries={["/workspaces/ws-1"]}>
        <OperatorProvider operator={false} securityOperator={false} principal="dana@corp.example">
          <Routes>
            <Route path="/workspaces/:id" element={<WorkspaceDetailScreen />} />
          </Routes>
        </OperatorProvider>
      </MemoryRouter>,
    );
    // The owner can mutate (Delete is live), which is exactly why Change must not follow canMutate.
    expect(await screen.findByRole("button", { name: /delete this workspace/i })).not.toBeDisabled();
    expect(changeButton()).toBeDisabled();
    expect(screen.getAllByText(OPERATOR_ONLY_REASON).length).toBeGreaterThan(0);
  });

  it("a non-operator never gets an enabled Change", async () => {
    getWorkspaceMock.mockResolvedValue(ws());
    renderDetail(false);
    await screen.findByText("None");
    expect(changeButton()).toBeDisabled();
  });

  it("a provider the server hid from this person shows a dash and the canon line, never an id", async () => {
    getWorkspaceMock.mockResolvedValue(ws({ llm_cred: { provider_unavailable: true } }));
    renderDetail(false);
    const row = (await screen.findByText(/^Model provider/)).closest("p");
    expect(row).toHaveTextContent("Model provider —");
    expect(screen.getAllByText(DENIED.WORKSPACE_NOT_AVAILABLE).length).toBeGreaterThan(0);
    expect(screen.queryByText(DESCRIPTION)).not.toBeInTheDocument();
  });
});
