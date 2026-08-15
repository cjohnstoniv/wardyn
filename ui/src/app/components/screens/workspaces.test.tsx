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

import { WorkspacesScreen, attentionItems, sourceSubLine } from "./workspaces";
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

  it("Model access column: the binding chip names the bound Integration", async () => {
    listWorkspacesMock.mockResolvedValue([
      ws({}, { status: "scanned", llm_cred: { integration_ref: "ai-anthropic-key" } }),
    ]);
    renderScreen();
    expect(await screen.findByText("ai-anthropic-key")).toBeInTheDocument();
  });

  it("Model access column: unbound reads 'Not pinned', not a bare 'None' — the run still falls back to a server default", async () => {
    listWorkspacesMock.mockResolvedValue([ws({}, { status: "scanned" })]);
    renderScreen();
    const chip = await screen.findByText("Not pinned");
    expect(chip).toHaveAttribute("title", "Runs fall back to the server's global provider.");
    expect(screen.queryByText("None")).not.toBeInTheDocument();
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

describe("WorkspacesScreen — kebab is Open · Edit workspace… · Delete… only", () => {
  beforeEach(() => {
    listWorkspacesMock.mockReset();
    listSecretsMock.mockReset().mockResolvedValue([]);
    // "Edit workspace…" now mounts the real WorkspaceWizard, whose mount
    // effect fetches these too (mirrors the "new wizard opens" block below).
    getSetupStatusMock.mockReset().mockResolvedValue({ secrets: { github_app: false } });
    listIntegrationsMock.mockReset().mockResolvedValue({ ai: [] });
  });

  it("offers exactly those three items — no Scan now, Resume import, Model access, or Env as code", async () => {
    listWorkspacesMock.mockResolvedValue([ws({}, { status: "error" })]);
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderScreen();
    await user.click(await screen.findByRole("button", { name: /workspace actions/i }));
    const menu = screen.getByRole("menu");
    expect(within(menu).getByRole("menuitem", { name: "Open" })).toBeInTheDocument();
    expect(within(menu).getByRole("menuitem", { name: /edit workspace/i })).toBeInTheDocument();
    expect(within(menu).getByRole("menuitem", { name: /delete/i })).toBeInTheDocument();
    expect(within(menu).queryByRole("menuitem", { name: /scan/i })).not.toBeInTheDocument();
    expect(within(menu).queryByRole("menuitem", { name: /resume import/i })).not.toBeInTheDocument();
    expect(within(menu).queryByRole("menuitem", { name: /model access/i })).not.toBeInTheDocument();
    expect(within(menu).queryByRole("menuitem", { name: /env as code/i })).not.toBeInTheDocument();
    expect(within(menu).queryByRole("menuitem", { name: /view profile/i })).not.toBeInTheDocument();
  });

  it("Edit workspace… opens the wizard hydrated on this row, not the legacy dialog", async () => {
    listWorkspacesMock.mockResolvedValue([ws({}, { status: "scanned" })]);
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderScreen();
    await user.click(await screen.findByRole("button", { name: /workspace actions/i }));
    await user.click(screen.getByRole("menuitem", { name: /edit workspace/i }));
    expect(await screen.findByRole("heading", { name: "Edit workspace" })).toBeInTheDocument();
    // The wizard's rail, not the old single-form dialog.
    expect(screen.getAllByText("Sources").length).toBeGreaterThan(0);
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

  it("the row is also a keyboard target — Tab then Enter opens the same route", async () => {
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
    const name = await screen.findByText("payments");
    const row = name.closest("tr") as HTMLElement;
    expect(row).toHaveAttribute("tabIndex", "0");
    row.focus();
    await user.keyboard("{Enter}");
    expect(await screen.findByText("detail for {id}")).toBeInTheDocument();
  });

  it("the kebab is a keyboard target too — Enter on it opens the menu, not the detail route", async () => {
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
    const kebab = await screen.findByRole("button", { name: /workspace actions/i });
    kebab.focus();
    await user.keyboard("{Enter}");
    expect(await screen.findByRole("menu")).toBeInTheDocument();
    expect(screen.queryByText("detail for {id}")).not.toBeInTheDocument();
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

describe("attentionItems / sourceSubLine — pure helpers", () => {
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

  it("sourceSubLine shows the mono source (+ ref for a repo) for a single-source workspace", () => {
    expect(sourceSubLine(ws({}, { kind: "repo", source: "acme/x", ref: "main" }))).toBe("acme/x @main");
    expect(sourceSubLine(ws({}, { kind: "local_dir", source: "/srv/x" }))).toBe("/srv/x");
  });

  // UI-LIB-8: ephemeral carries neither Path nor Source (store.go's
  // deriveWorkspaceMirrors), so ws.source is always "" — the row must name
  // the kind instead of rendering a blank mono line.
  it("sourceSubLine names the kind for an ephemeral workspace, whose Source is always empty", () => {
    expect(sourceSubLine(ws({}, { kind: "ephemeral", source: "" }))).toBe("ephemeral — scratch space");
  });
});

// WorkspaceLLMCredDialog — the standalone editor for an EXISTING workspace's
// binding (the onboarding form's llm_cred is create-only; this is the only
// path that can change it afterward — PUT /workspaces/{id}/llm-cred).
describe("WorkspaceLLMCredDialog", () => {
  beforeEach(() => {
    setWorkspaceLLMCredMock.mockReset();
    listSecretsMock.mockReset().mockResolvedValue([]);
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
