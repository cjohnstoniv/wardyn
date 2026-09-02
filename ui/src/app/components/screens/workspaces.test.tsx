/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { Workspace, WorkspaceProfile } from "../../lib/types";

// The workspaces LIST: a single table, four columns (Workspace / Source /
// Image / Model) + an overflow kebab. Stage 2 dropped the tier tabs (Sources
// library / Base image catalog), the Status column, and the "Needs you"
// column — this file tests what's left.

const createWorkspaceMock = vi.fn();
const setWorkspaceLLMCredMock = vi.fn();
const listWorkspacesMock = vi.fn();
const deleteWorkspaceMock = vi.fn();
vi.mock("../../lib/api/workspaces", () => ({
  workspaces: {
    createWorkspace: (...a: unknown[]) => createWorkspaceMock(...a),
    setWorkspaceLLMCred: (...a: unknown[]) => setWorkspaceLLMCredMock(...a),
    listWorkspaces: (...a: unknown[]) => listWorkspacesMock(...a),
    deleteWorkspace: (...a: unknown[]) => deleteWorkspaceMock(...a),
  },
}));
const listIntegrationsMock = vi.fn();
vi.mock("../../lib/api/integrations", async () => {
  const actual = await vi.importActual<typeof import("../../lib/api/integrations")>("../../lib/api/integrations");
  return { ...actual, integrationsApi: { list: (...a: unknown[]) => listIntegrationsMock(...a) } };
});
vi.mock("sonner", () => ({
  toast: { error: vi.fn(), success: vi.fn(), info: vi.fn() },
}));

import { WorkspacesScreen, sourceSubLine, workspaceImage } from "./workspaces";
import { WorkspaceLLMCredDialog } from "./workspace-llm-cred";
import { OperatorProvider, RoleProvider } from "../wardyn/operator-context";
import { DRIVES } from "../../lib/user-drives-copy";

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
    status: "scanned",
    created_at: "",
    updated_at: "",
    profile: profile as unknown as Record<string, unknown>,
    ...over,
  };
}

describe("WorkspacesScreen — list columns", () => {
  beforeEach(() => {
    listWorkspacesMock.mockReset();
  });

  it("Workspace + Source columns: the kind icon and name in one cell, the mono sub-line in another", async () => {
    listWorkspacesMock.mockResolvedValue([ws({}, { name: "payments", kind: "repo", source: "acme/payments", ref: "main", status: "scanned" })]);
    renderScreen();
    expect(await screen.findByText("payments")).toBeInTheDocument();
    expect(screen.getByText("acme/payments @main")).toBeInTheDocument();
  });

  it("Source column: a multi-source workspace shows the composition summary instead", async () => {
    const w = ws({}, { status: "scanned" }) as unknown as Workspace & { sources: unknown[] };
    w.sources = [{ type: "local_dir", path: "/a" }, { type: "local_dir", path: "/b" }, { type: "repo", source: "acme/x" }];
    listWorkspacesMock.mockResolvedValue([w]);
    renderScreen();
    expect(await screen.findByText("2 dirs · 1 repo")).toBeInTheDocument();
  });

  it("Image column: standard sandbox image when nothing was detected and nothing was pinned", async () => {
    listWorkspacesMock.mockResolvedValue([ws({}, { status: "scanned" })]);
    renderScreen();
    expect(await screen.findByText("standard sandbox image")).toBeInTheDocument();
  });

  it("Image column: devcontainer.json when the scan detected one", async () => {
    listWorkspacesMock.mockResolvedValue([ws({ has_devcontainer: true }, { status: "scanned" })]);
    renderScreen();
    expect(await screen.findByText("devcontainer.json")).toBeInTheDocument();
  });

  it("Image column: the mono ref when an explicit image is pinned — never a base-image catalog fetch", async () => {
    listWorkspacesMock.mockResolvedValue([
      ws({}, { status: "scanned", base_image: { kind: "byo", image: "ghcr.io/acme/dev@sha256:ab12" } }),
    ]);
    renderScreen();
    expect(await screen.findByText("ghcr.io/acme/dev@sha256:ab12")).toBeInTheDocument();
  });

  it("Model column: the binding chip names the bound Integration", async () => {
    listWorkspacesMock.mockResolvedValue([
      ws({}, { status: "scanned", llm_cred: { integration_ref: "ai-anthropic-key" } }),
    ]);
    renderScreen();
    expect(await screen.findByText("ai-anthropic-key")).toBeInTheDocument();
  });

  it("Model column: unbound reads a bare em dash", async () => {
    listWorkspacesMock.mockResolvedValue([ws({}, { status: "scanned" })]);
    renderScreen();
    await screen.findByText("payments");
    expect(screen.getByText("—")).toBeInTheDocument();
  });
});

describe("WorkspacesScreen — kebab is Open · Delete… only", () => {
  beforeEach(() => {
    listWorkspacesMock.mockReset();
  });

  it("offers exactly those two items — no Edit workspace, no Scan", async () => {
    listWorkspacesMock.mockResolvedValue([ws({}, {})]);
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderScreen();
    await user.click(await screen.findByRole("button", { name: /workspace actions/i }));
    const menu = screen.getByRole("menu");
    expect(within(menu).getByRole("menuitem", { name: "Open" })).toBeInTheDocument();
    expect(within(menu).getByRole("menuitem", { name: /delete/i })).toBeInTheDocument();
    expect(within(menu).queryByRole("menuitem", { name: /edit workspace/i })).not.toBeInTheDocument();
    expect(within(menu).queryByRole("menuitem", { name: /scan/i })).not.toBeInTheDocument();
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

  it("the row is also a keyboard target — Tab then Enter opens the same route", async () => {
    listWorkspacesMock.mockReset().mockResolvedValue([ws({}, { id: "ws-42", status: "scanned" })]);
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
    const name = await screen.findByText("payments");
    const row = name.closest("tr") as HTMLElement;
    expect(row).toHaveAttribute("tabIndex", "0");
    row.focus();
    await user.keyboard("{Enter}");
    expect(await screen.findByText("detail for {id}")).toBeInTheDocument();
  });

  it("the kebab is a keyboard target too — Enter on it opens the menu, not the detail route", async () => {
    listWorkspacesMock.mockReset().mockResolvedValue([ws({}, { id: "ws-42", status: "scanned" })]);
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
    const kebab = await screen.findByRole("button", { name: /workspace actions/i });
    kebab.focus();
    await user.keyboard("{Enter}");
    expect(await screen.findByRole("menu")).toBeInTheDocument();
    expect(screen.queryByText("detail for {id}")).not.toBeInTheDocument();
  });
});

describe("WorkspacesScreen — Add workspace dialog opens from both the header button and the empty state", () => {
  beforeEach(() => {
    listWorkspacesMock.mockReset();
  });

  it("the empty state's 'Add your first workspace' opens the ONE add-workspace dialog", async () => {
    listWorkspacesMock.mockResolvedValue([]);
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderScreen();
    await user.click(await screen.findByRole("button", { name: /add your first workspace/i }));
    expect(await screen.findByRole("heading", { name: "Add workspace" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Repository" })).toBeInTheDocument();
  });

  it("the header's '+ Add workspace' opens the SAME dialog", async () => {
    listWorkspacesMock.mockResolvedValue([ws({}, { status: "scanned" })]);
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderScreen();
    await screen.findByText("payments");
    await user.click(screen.getByRole("button", { name: /add workspace/i }));
    expect(await screen.findByRole("heading", { name: "Add workspace" })).toBeInTheDocument();
  });
});

// M3 (0027f514): POST /workspaces is member-allowed now — the header "Add
// workspace" button used to be operator-only. Pin that a MEMBER role sees it
// enabled, not gated behind the operator-only chip/disabled state.
describe("WorkspacesScreen — M3 member workspace access", () => {
  beforeEach(() => {
    listWorkspacesMock.mockReset().mockResolvedValue([]);
    createWorkspaceMock.mockReset();
  });

  function renderAsMember() {
    return render(
      <OperatorProvider operator={false} memberLocalDirRoot="/home/agent-projects">
        <RoleProvider role="member">
          <MemoryRouter>
            <WorkspacesScreen />
          </MemoryRouter>
        </RoleProvider>
      </OperatorProvider>,
    );
  }

  it("a member sees the header 'Add workspace' button enabled", async () => {
    renderAsMember();
    expect(await screen.findByRole("button", { name: "Add workspace" })).toBeEnabled();
  });

  // add-workspace-dialog.tsx:168 — the writable checkbox unmounts (not
  // resets) once role==="member" && kind==="local_dir", so a box checked
  // while Repository was still selected must not ride along into the
  // submitted local_dir source.
  it("a member's local_dir submit drops writable even if it was checked under Repository first", async () => {
    createWorkspaceMock.mockResolvedValue(ws({}, { id: "ws-new", kind: "local_dir" }));
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderAsMember();

    await user.click(await screen.findByRole("button", { name: "Add workspace" }));
    const dialog = await screen.findByRole("dialog");

    await user.click(within(dialog).getByRole("button", { name: /^Advanced/ }));
    await user.click(within(dialog).getByRole("checkbox", { name: /allow writes/i }));
    await user.click(within(dialog).getByRole("button", { name: "Local directory" }));
    await user.type(within(dialog).getByLabelText("Path on this host"), "/home/agent-projects/payments");
    await user.click(within(dialog).getByRole("button", { name: "Add workspace" }));

    await waitFor(() => expect(createWorkspaceMock).toHaveBeenCalled());
    const [payload] = createWorkspaceMock.mock.calls[0] as [{ sources: Array<{ writable?: boolean }> }];
    expect(payload.sources[0].writable).toBeUndefined();
  });
});

describe("sourceSubLine / workspaceImage — pure helpers", () => {
  it("sourceSubLine shows the mono source (+ ref for a repo) for a single-source workspace", () => {
    expect(sourceSubLine(ws({}, { kind: "repo", source: "acme/x", ref: "main" }))).toBe("acme/x @main");
    expect(sourceSubLine(ws({}, { kind: "local_dir", source: "/srv/x" }))).toBe("/srv/x");
  });

  // UI-LIB-8: ephemeral carries neither Path nor Source (store.go's
  // deriveWorkspaceMirrors), so ws.source is always "" — the row must read
  // the CANON copy, not a blank mono line.
  it("sourceSubLine reads 'empty — discarded after the run' for an ephemeral workspace", () => {
    expect(sourceSubLine(ws({}, { kind: "ephemeral", source: "" }))).toBe("empty — discarded after the run");
  });

  it("workspaceImage prefers an explicit pinned ref over a detected devcontainer", () => {
    const w = ws({ has_devcontainer: true }, { base_image: { kind: "registry", image: "mcr.microsoft.com/x" } });
    expect(workspaceImage(w)).toEqual({ kind: "ref", label: "mcr.microsoft.com/x" });
  });

  it("workspaceImage falls back to standard sandbox image when nothing was detected or pinned", () => {
    expect(workspaceImage(ws({}))).toEqual({ kind: "standard", label: "standard sandbox image" });
  });
});

// WorkspaceLLMCredDialog — the standalone editor for an EXISTING workspace's
// binding (the onboarding form's llm_cred is create-only; this is the only
// path that can change it afterward — PUT /workspaces/{id}/llm-cred).
describe("WorkspaceLLMCredDialog", () => {
  beforeEach(() => {
    setWorkspaceLLMCredMock.mockReset();
    listIntegrationsMock.mockReset().mockResolvedValue({ ai: [], scm: [] });
  });

  it("saves the picked Integration via setWorkspaceLLMCred and reports the updated workspace", async () => {
    listIntegrationsMock.mockResolvedValue({
      // serverId is required: LLMCredFields skips any row without one (a
      // deliberate filter — see workspace-llm-cred.tsx's `.filter((r) =>
      // r.serverId)` — mirroring step-access.tsx's identical picker, since a
      // client display id with no server-side identity would silently fail
      // to bind server-side). A real deriveAiRows "Managed subscription" row
      // always carries one (aiServerId("anthropic_subscription", false)).
      ai: [{ id: "ai-managed", serverId: "ai-managed", name: "Managed subscription", typeLabel: "anthropic · managed login" }],
      scm: [],
    });
    const workspace = ws({}, { id: "ws-9", name: "payments", llm_cred: {} });
    const updated = { ...workspace, llm_cred: { integration_ref: "ai-managed" } };
    setWorkspaceLLMCredMock.mockResolvedValue(updated);
    const onSaved = vi.fn();
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    render(<WorkspaceLLMCredDialog workspace={workspace} onOpenChange={vi.fn()} onSaved={onSaved} />);

    await user.click(await screen.findByRole("radio", { name: /managed subscription/i }));
    await user.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() =>
      expect(setWorkspaceLLMCredMock).toHaveBeenCalledWith("ws-9", { integration_ref: "ai-managed" }),
    );
    await waitFor(() => expect(onSaved).toHaveBeenCalledWith(updated));
  });

  it("keeps a stored ref that no longer lists selectable — named honestly, never invented", async () => {
    listIntegrationsMock.mockResolvedValue({ ai: [], scm: [] });
    const workspace = ws({}, { llm_cred: { integration_ref: "ai-gone" } });
    render(<WorkspaceLLMCredDialog workspace={workspace} onOpenChange={vi.fn()} onSaved={vi.fn()} />);
    expect(await screen.findByRole("radio", { name: /ai-gone \(not in the Integrations list\)/i })).toBeChecked();
  });
});

// The /drives door (0.7 user drives, prompt §6). This screen has no nav item to
// offer and neither does /drives, so the header's outline button IS the entry
// point — and it is SUPER-only, because a member and a security admin have
// nothing to act on there.
describe("WorkspacesScreen — the User drives door", () => {
  beforeEach(() => {
    listWorkspacesMock.mockReset().mockResolvedValue([ws({}, { status: "scanned" })]);
  });

  it("an operator gets it beside Add workspace, outline — the teal stays on Add workspace", async () => {
    renderScreen();
    const door = await screen.findByRole("button", { name: DRIVES.TITLE });
    expect(door.className.split(/\s+/)).not.toContain("bg-primary");
    expect(screen.getByRole("button", { name: /add workspace/i }).className.split(/\s+/)).toContain("bg-primary");
  });

  it("a member never sees it — /drives is SUPER and the entry points say so", async () => {
    render(
      <OperatorProvider operator={false}>
        <RoleProvider role="member">
          <MemoryRouter>
            <WorkspacesScreen />
          </MemoryRouter>
        </RoleProvider>
      </OperatorProvider>,
    );
    await screen.findByRole("button", { name: /add workspace/i });
    expect(screen.queryByRole("button", { name: DRIVES.TITLE })).not.toBeInTheDocument();
  });
});
