/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { Workspace, WorkspaceProfile } from "../../lib/types";

// The workspaces LIST: what a row says about a workspace at a glance (its
// composition, one status word, its model binding, and whether it needs the
// operator), plus the two dialogs that still live here. The scan profile's own
// rendering moved to the requirements surfaces (the wizard's step 3 and the
// detail page's card), which is where its honesty invariants are now tested.

const setApprovedEgressMock = vi.fn();
const getObservedEgressMock = vi.fn();
const createWorkspaceMock = vi.fn();
const setWorkspaceLLMCredMock = vi.fn();
const listWorkspacesMock = vi.fn();
const getEnvAsCodeMock = vi.fn();
const deleteWorkspaceMock = vi.fn();
const updateWorkspaceMock = vi.fn();
vi.mock("../../lib/api/workspaces", () => ({
  workspaces: {
    setApprovedEgress: (...a: unknown[]) => setApprovedEgressMock(...a),
    getObservedEgress: (...a: unknown[]) => getObservedEgressMock(...a),
    createWorkspace: (...a: unknown[]) => createWorkspaceMock(...a),
    setWorkspaceLLMCred: (...a: unknown[]) => setWorkspaceLLMCredMock(...a),
    listWorkspaces: (...a: unknown[]) => listWorkspacesMock(...a),
    getEnvAsCode: (...a: unknown[]) => getEnvAsCodeMock(...a),
    deleteWorkspace: (...a: unknown[]) => deleteWorkspaceMock(...a),
    updateWorkspace: (...a: unknown[]) => updateWorkspaceMock(...a),
  },
}));
const listSecretsMock = vi.fn();
vi.mock("../../lib/api/secrets", () => ({
  secrets: { listSecrets: (...a: unknown[]) => listSecretsMock(...a) },
}));
// The wizard mounted from "+ Add workspace" fetches these on mount too — both
// swallow their own rejection (see wizard.tsx), but mocking them keeps the
// wizard-opens test quiet and mirrors wizard.test.tsx's own convention.
const getSetupStatusMock = vi.fn();
vi.mock("../../lib/api/setup", () => ({
  setup: { getSetupStatus: (...a: unknown[]) => getSetupStatusMock(...a) },
}));
const listIntegrationsMock = vi.fn();
vi.mock("../../lib/api/integrations", async () => {
  const actual = await vi.importActual<typeof import("../../lib/api/integrations")>("../../lib/api/integrations");
  return { ...actual, integrationsApi: { list: (...a: unknown[]) => listIntegrationsMock(...a) } };
});
vi.mock("sonner", () => ({
  toast: { error: vi.fn(), success: vi.fn(), info: vi.fn() },
}));

import { AddWorkspaceDialog, WorkspacesScreen, attentionItems, modelAccessBroken, sourceSubLine } from "./workspaces";
import { WorkspaceLLMCredDialog } from "./workspace-llm-cred";

function renderScreen() {
  return render(
    <MemoryRouter>
      <WorkspacesScreen />
    </MemoryRouter>,
  );
}

function ws(profile: WorkspaceProfile, over: Partial<Workspace> = {}): Workspace {
  return {
    id: "ws-1",
    name: "payments",
    kind: "local_dir",
    source: "/srv/payments",
    status: "ready",
    created_at: "",
    updated_at: "",
    profile: profile as unknown as Record<string, unknown>,
    ...over,
  };
}


describe("WorkspacesScreen — list columns", () => {
  beforeEach(() => {
    listWorkspacesMock.mockReset();
    listSecretsMock.mockReset().mockResolvedValue([]);
  });

  it("Workspace column: kind icon, name, and the single-source mono sub-line", async () => {
    listWorkspacesMock.mockResolvedValue([ws({}, { name: "payments", kind: "repo", source: "acme/payments", ref: "main", status: "scanned" })]);
    renderScreen();
    expect(await screen.findByText("payments")).toBeInTheDocument();
    expect(screen.getByText("acme/payments @main")).toBeInTheDocument();
  });

  it("Workspace column: a multi-source workspace shows the composition summary instead", async () => {
    const w = ws({}, { status: "scanned" }) as unknown as Workspace & { sources: unknown[] };
    w.sources = [{ type: "local_dir", path: "/a" }, { type: "local_dir", path: "/b" }, { type: "repo", source: "acme/x" }];
    listWorkspacesMock.mockResolvedValue([w]);
    renderScreen();
    expect(await screen.findByText("2 dirs · 1 repo")).toBeInTheDocument();
  });

  it("Status column: ONE chip from the shared statusWord vocabulary", async () => {
    listWorkspacesMock.mockResolvedValue([ws({}, { status: "error" })]);
    renderScreen();
    expect(await screen.findByText("Scan failed")).toBeInTheDocument();
  });

  it("Model access column: the binding chip, plus a warning dot when its secret isn't stored", async () => {
    listWorkspacesMock.mockResolvedValue([
      ws({}, { status: "scanned", llm_cred: { mode: "api_key", api_key_secret: "missing-key" } }),
    ]);
    renderScreen();
    expect(await screen.findByText("API key: missing-key")).toBeInTheDocument();
    expect(screen.getByRole("img", { name: /bound secret isn't in the store/i })).toBeInTheDocument();
  });

  it("Model access column: no warning dot once the bound secret is stored", async () => {
    listWorkspacesMock.mockResolvedValue([
      ws({}, { status: "scanned", llm_cred: { mode: "api_key", api_key_secret: "present-key" } }),
    ]);
    listSecretsMock.mockResolvedValue(["present-key"]);
    renderScreen();
    await screen.findByText("API key: present-key");
    expect(screen.queryByRole("img", { name: /bound secret isn't in the store/i })).not.toBeInTheDocument();
  });

  it("Needs you: unstored required secrets, hosts awaiting review, then suspected leaks, in that order", async () => {
    const w = ws(
      {
        egress_domains: [],
        suggested_egress: ["telemetry.acme.io"],
        leak_findings: [{ path: "src/config.ts", kind: "aws-access-key" }],
      },
      { status: "scanned" },
    );
    (w as unknown as { requirements: Record<string, { level: string; provenance: string }> }).requirements = {
      "secret:DATABASE_URL": { level: "required", provenance: "scan_seeded" },
    };
    listWorkspacesMock.mockResolvedValue([w]);
    renderScreen();
    const secretLine = await screen.findByText("1 secret not stored");
    const cell = secretLine.closest("td")!;
    expect(within(cell).getByText("1 host awaiting review")).toBeInTheDocument();
    expect(within(cell).getByText("⚠ 1 suspected committed secret")).toBeInTheDocument();
  });

  it("Needs you: an em dash when nothing needs attention", async () => {
    listWorkspacesMock.mockResolvedValue([ws({}, { status: "scanned" })]);
    renderScreen();
    await screen.findByText("payments");
    expect(screen.getByText("—")).toBeInTheDocument();
  });
});

describe("WorkspacesScreen — kebab is Open · Edit source… · Delete… only", () => {
  beforeEach(() => {
    listWorkspacesMock.mockReset();
    listSecretsMock.mockReset().mockResolvedValue([]);
  });

  it("offers exactly those three items — no Scan now, Resume import, Model access, or Env as code", async () => {
    listWorkspacesMock.mockResolvedValue([ws({}, { status: "error" })]);
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderScreen();
    await user.click(await screen.findByRole("button", { name: /workspace actions/i }));
    const menu = screen.getByRole("menu");
    expect(within(menu).getByRole("menuitem", { name: "Open" })).toBeInTheDocument();
    expect(within(menu).getByRole("menuitem", { name: /edit source/i })).toBeInTheDocument();
    expect(within(menu).getByRole("menuitem", { name: /delete/i })).toBeInTheDocument();
    expect(within(menu).queryByRole("menuitem", { name: /scan/i })).not.toBeInTheDocument();
    expect(within(menu).queryByRole("menuitem", { name: /resume import/i })).not.toBeInTheDocument();
    expect(within(menu).queryByRole("menuitem", { name: /model access/i })).not.toBeInTheDocument();
    expect(within(menu).queryByRole("menuitem", { name: /env as code/i })).not.toBeInTheDocument();
    expect(within(menu).queryByRole("menuitem", { name: /view profile/i })).not.toBeInTheDocument();
  });

  it("Edit source… still opens the existing edit form (reused, unchanged)", async () => {
    listWorkspacesMock.mockResolvedValue([ws({}, { status: "scanned" })]);
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderScreen();
    await user.click(await screen.findByRole("button", { name: /workspace actions/i }));
    await user.click(screen.getByRole("menuitem", { name: /edit source/i }));
    expect(await screen.findByText("Edit workspace")).toBeInTheDocument();
  });

  it("Delete… deletes via the existing confirm dialog", async () => {
    listWorkspacesMock.mockResolvedValue([ws({}, { status: "scanned" })]);
    deleteWorkspaceMock.mockReset().mockResolvedValue(undefined);
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderScreen();
    await user.click(await screen.findByRole("button", { name: /workspace actions/i }));
    await user.click(screen.getByRole("menuitem", { name: /delete/i }));
    await user.click(await screen.findByRole("button", { name: /delete workspace/i }));
    await waitFor(() => expect(deleteWorkspaceMock).toHaveBeenCalledWith("ws-1"));
  });
});

describe("WorkspacesScreen — row click navigates to the detail route", () => {
  it("clicking a row (not the kebab) opens /workspaces/:id", async () => {
    listWorkspacesMock.mockReset().mockResolvedValue([ws({}, { id: "ws-42", status: "scanned" })]);
    listSecretsMock.mockReset().mockResolvedValue([]);
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    const { Route, Routes } = await import("react-router-dom");
    render(
      <MemoryRouter initialEntries={["/workspaces"]}>
        <Routes>
          <Route path="/workspaces" element={<WorkspacesScreen />} />
          <Route path="/workspaces/:id" element={<div>detail for {"{id}"}</div>} />
        </Routes>
      </MemoryRouter>,
    );
    await user.click(await screen.findByText("payments"));
    expect(await screen.findByText("detail for {id}")).toBeInTheDocument();
  });
});

describe("WorkspacesScreen — the new wizard opens from both the header button and the empty state", () => {
  beforeEach(() => {
    listWorkspacesMock.mockReset();
    listSecretsMock.mockReset().mockResolvedValue([]);
    getSetupStatusMock.mockReset().mockResolvedValue({ secrets: { github_app: false } });
    listIntegrationsMock.mockReset().mockResolvedValue({ ai: [] });
  });

  it("the empty state's 'Onboard your first workspace' opens the four-step wizard", async () => {
    listWorkspacesMock.mockResolvedValue([]);
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderScreen();
    await user.click(await screen.findByRole("button", { name: /onboard your first workspace/i }));
    expect(await screen.findByRole("heading", { name: "Add workspace" })).toBeInTheDocument();
    expect(screen.getAllByText("Sources").length).toBeGreaterThan(0);
  });

  it("the header's '+ Add workspace' opens the SAME wizard", async () => {
    listWorkspacesMock.mockResolvedValue([ws({}, { status: "scanned" })]);
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderScreen();
    await screen.findByText("payments");
    await user.click(screen.getByRole("button", { name: /add workspace/i }));
    expect(await screen.findByRole("heading", { name: "Add workspace" })).toBeInTheDocument();
  });
});

describe("attentionItems / modelAccessBroken / sourceSubLine — pure helpers", () => {
  it("attentionItems orders unstored secrets, then pending hosts, then leaks", () => {
    const w = ws({ suggested_egress: ["h.example.com"], leak_findings: [{ path: "x", kind: "y" }] });
    (w as unknown as { requirements: Record<string, { level: string; provenance: string }> }).requirements = {
      "secret:A": { level: "required", provenance: "scan_seeded" },
    };
    expect(attentionItems(w, [])).toEqual([
      { text: "1 secret not stored", tone: "warning" },
      { text: "1 host awaiting review", tone: "neutral" },
      { text: "⚠ 1 suspected committed secret", tone: "danger" },
    ]);
  });

  it("attentionItems is empty once nothing is pending", () => {
    expect(attentionItems(ws({}), [])).toEqual([]);
  });

  it("modelAccessBroken is true only for an api_key binding whose secret is missing", () => {
    expect(modelAccessBroken(ws({}, { llm_cred: { mode: "api_key", api_key_secret: "x" } }), [])).toBe(true);
    expect(modelAccessBroken(ws({}, { llm_cred: { mode: "api_key", api_key_secret: "x" } }), ["x"])).toBe(false);
    expect(modelAccessBroken(ws({}, { llm_cred: { mode: "managed" } }), [])).toBe(false);
  });

  it("sourceSubLine shows the mono source (+ ref for a repo) for a single-source workspace", () => {
    expect(sourceSubLine(ws({}, { kind: "repo", source: "acme/x", ref: "main" }))).toBe("acme/x @main");
    expect(sourceSubLine(ws({}, { kind: "local_dir", source: "/srv/x" }))).toBe("/srv/x");
  });
});

// AddWorkspaceDialog — onboarding a "container" kind (image ref, no host mount)
// and binding a model/harness credential at create time.
describe("AddWorkspaceDialog — container kind + model/harness binding", () => {
  beforeEach(() => {
    createWorkspaceMock.mockReset();
    listSecretsMock.mockReset().mockResolvedValue([]);
  });

  it("onboards a container by image ref, with no writable/default-target fields", async () => {
    createWorkspaceMock.mockResolvedValue(ws({}, { kind: "container", source: "ubuntu:24.04" }));
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    const onSaved = vi.fn();

    render(<AddWorkspaceDialog open onOpenChange={vi.fn()} onSaved={onSaved} />);

    await user.click(screen.getByRole("radio", { name: /container image/i }));
    // Source field relabels to "Image ref" and drops local_dir's absolute-path rule.
    expect(screen.getByText("Image ref")).toBeInTheDocument();
    // A container has no host mount — the writable opt-in (local_dir only) is gone.
    expect(screen.queryByLabelText(/let agents write to this directory/i)).toBeNull();
    expect(screen.queryByLabelText(/default target/i)).toBeNull();

    await user.type(screen.getByLabelText("Name"), "sandbox-env");
    await user.type(screen.getByPlaceholderText("ubuntu:24.04"), "ubuntu:24.04");
    await user.click(screen.getByRole("button", { name: "Add workspace" }));

    await waitFor(() =>
      expect(createWorkspaceMock).toHaveBeenCalledWith(
        expect.objectContaining({ kind: "container", source: "ubuntu:24.04", default_target: undefined, writable: undefined }),
      ),
    );
    await waitFor(() => expect(onSaved).toHaveBeenCalled());
  });

  it("includes an api_key model/harness binding in the create payload when selected", async () => {
    createWorkspaceMock.mockResolvedValue(ws({}));
    listSecretsMock.mockResolvedValue(["acme-anthropic-key"]);
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    render(<AddWorkspaceDialog open onOpenChange={vi.fn()} onSaved={vi.fn()} />);

    await user.type(screen.getByLabelText("Name"), "payments");
    await user.type(screen.getByPlaceholderText("/home/me/projects/payments"), "/srv/payments");
    await user.click(screen.getByRole("radio", { name: "API key" }));
    await user.type(screen.getByPlaceholderText("anthropic-api-key"), "acme-anthropic-key");
    await user.click(screen.getByRole("button", { name: "Add workspace" }));

    await waitFor(() =>
      expect(createWorkspaceMock).toHaveBeenCalledWith(
        expect.objectContaining({ llm_cred: { mode: "api_key", api_key_secret: "acme-anthropic-key" } }),
      ),
    );
  });

  it("omits llm_cred entirely when the binding is left at None", async () => {
    createWorkspaceMock.mockResolvedValue(ws({}));
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    render(<AddWorkspaceDialog open onOpenChange={vi.fn()} onSaved={vi.fn()} />);
    await user.type(screen.getByLabelText("Name"), "payments");
    await user.type(screen.getByPlaceholderText("/home/me/projects/payments"), "/srv/payments");
    await user.click(screen.getByRole("button", { name: "Add workspace" }));

    await waitFor(() => expect(createWorkspaceMock).toHaveBeenCalled());
    expect(createWorkspaceMock.mock.calls[0][0].llm_cred).toBeUndefined();
  });
});

// WorkspaceLLMCredDialog — the standalone editor for an EXISTING workspace's
// binding (the onboarding form's llm_cred is create-only; this is the only
// path that can change it afterward — PUT /workspaces/{id}/llm-cred).
describe("WorkspaceLLMCredDialog", () => {
  beforeEach(() => {
    setWorkspaceLLMCredMock.mockReset();
    listSecretsMock.mockReset().mockResolvedValue([]);
  });

  it("saves the selected mode via setWorkspaceLLMCred and reports the updated workspace", async () => {
    const workspace = ws({}, { id: "ws-9", name: "payments", llm_cred: { mode: "" } });
    const updated = { ...workspace, llm_cred: { mode: "managed" as const } };
    setWorkspaceLLMCredMock.mockResolvedValue(updated);
    const onSaved = vi.fn();
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    render(<WorkspaceLLMCredDialog workspace={workspace} onOpenChange={vi.fn()} onSaved={onSaved} />);

    await user.click(screen.getByRole("radio", { name: /managed/i }));
    await user.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => expect(setWorkspaceLLMCredMock).toHaveBeenCalledWith("ws-9", { mode: "managed" }));
    await waitFor(() => expect(onSaved).toHaveBeenCalledWith(updated));
  });

  it("preloads the workspace's existing binding", () => {
    const workspace = ws({}, { llm_cred: { mode: "bedrock", bedrock: { region: "us-east-1" } } });
    render(<WorkspaceLLMCredDialog workspace={workspace} onOpenChange={vi.fn()} onSaved={vi.fn()} />);
    expect(screen.getByPlaceholderText("Region")).toHaveValue("us-east-1");
  });
});
