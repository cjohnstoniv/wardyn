/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { Workspace, WorkspaceProfile } from "../../../lib/types";

const setRequirementsMock = vi.fn();
const setWorkspaceLLMCredMock = vi.fn();
vi.mock("../../../lib/api/workspaces", () => ({
  workspaces: {
    setRequirements: (...a: unknown[]) => setRequirementsMock(...a),
    setWorkspaceLLMCred: (...a: unknown[]) => setWorkspaceLLMCredMock(...a),
  },
}));
const listSecretsMock = vi.fn().mockResolvedValue([]);
vi.mock("../../../lib/api/secrets", () => ({
  secrets: { listSecrets: (...a: unknown[]) => listSecretsMock(...a) },
}));
vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn(), info: vi.fn() } }));
const listIntegrationsMock = vi.fn().mockResolvedValue({
  ai: [{ id: "ai-managed", name: "Managed subscription", typeLabel: "anthropic · managed login" }],
  scm: [],
});
vi.mock("../../../lib/api/integrations", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/integrations")>(
    "../../../lib/api/integrations",
  );
  return { ...actual, integrationsApi: { list: (...a: unknown[]) => listIntegrationsMock(...a) } };
});

import { RequirementsCard } from "./requirements-card";
import { RD2 } from "../../../lib/workspace-copy";

// Radix Tabs activates a trigger on mousedown (not click) — fireEvent.click
// alone never fires that, so switching tabs needs real userEvent.
async function openTab(name: string) {
  const user = userEvent.setup({ pointerEventsCheck: 0 });
  await user.click(screen.getByRole("tab", { name }));
}

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

beforeEach(() => {
  setRequirementsMock.mockReset();
  setWorkspaceLLMCredMock.mockReset();
});

describe("RequirementsCard — model access is the first group", () => {
  it("shows 'Inherits global' style label when nothing is bound, before any contract group", () => {
    render(
      <RequirementsCard ws={ws()} storedSecretNames={[]} onWorkspaceUpdated={vi.fn()} onSecretStored={vi.fn()} />,
    );
    const heading = screen.getByText("Model access");
    expect(heading).toBeInTheDocument();
    expect(screen.getByText("None")).toBeInTheDocument();
    // The model-access group's own heading precedes the contract group headings
    // in document order (jsdom keeps source order; compareDocumentPosition
    // confirms it rather than assuming array order).
    const secretsHeading = screen.queryByText("Secrets");
    if (secretsHeading) {
      expect(heading.compareDocumentPosition(secretsHeading) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    }
  });

  it("opens the model access dialog and reports the saved workspace", async () => {
    const onWorkspaceUpdated = vi.fn();
    setWorkspaceLLMCredMock.mockResolvedValue(ws({ llm_cred: { integration_ref: "ai-managed" } }));
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    render(
      <RequirementsCard ws={ws()} storedSecretNames={[]} onWorkspaceUpdated={onWorkspaceUpdated} onSecretStored={vi.fn()} />,
    );
    await user.click(screen.getByRole("button", { name: /bind model access/i }));
    await user.click(await screen.findByRole("radio", { name: /managed subscription/i }));
    await user.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() =>
      expect(setWorkspaceLLMCredMock).toHaveBeenCalledWith("ws-1", { integration_ref: "ai-managed" }),
    );
    await waitFor(() => expect(onWorkspaceUpdated).toHaveBeenCalled());
  });
});

describe("RequirementsCard — reuses the wizard's StepRequirements and persists edits immediately", () => {
  const profile: WorkspaceProfile = {
    required_secrets: [{ name: "DATABASE_URL", kind: "postgres" }],
    egress_domains: ["registry.npmjs.org"],
  };

  it("renders the same contract groups the wizard renders", async () => {
    render(
      <RequirementsCard
        ws={ws({ profile: profile as unknown as Record<string, unknown> })}
        storedSecretNames={[]}
        onWorkspaceUpdated={vi.fn()}
        onSecretStored={vi.fn()}
      />,
    );
    // The Requirements step opens on Reach now (dependency order — the
    // record-last redesign); Secrets needs switching to.
    expect(screen.getByTestId("group-reach")).toBeInTheDocument();
    expect(screen.getByText("registry.npmjs.org")).toBeInTheDocument();
    await openTab("Secrets");
    expect(screen.getByTestId("group-secrets")).toBeInTheDocument();
    expect(screen.getByText("DATABASE_URL")).toBeInTheDocument();
  });

  it("flipping a lane calls setRequirements immediately (no separate save step)", async () => {
    const onWorkspaceUpdated = vi.fn();
    const updated = ws({ profile: profile as unknown as Record<string, unknown> });
    setRequirementsMock.mockResolvedValue(updated);
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    render(
      <RequirementsCard
        ws={ws({ profile: profile as unknown as Record<string, unknown> })}
        storedSecretNames={["database-url"]}
        onWorkspaceUpdated={onWorkspaceUpdated}
        onSecretStored={vi.fn()}
      />,
    );
    await user.click(screen.getByRole("tab", { name: "Secrets" }));
    const secretsGroup = within(screen.getByTestId("group-secrets"));
    await user.click(secretsGroup.getByRole("radio", { name: "Optional" }));
    await waitFor(() =>
      expect(setRequirementsMock).toHaveBeenCalledWith(
        "ws-1",
        expect.objectContaining({
          // Keyed by the STORABLE name (the server grammar's shape), while the
          // row still displays the detected env-var name.
          "secret:database-url": { level: "optional", provenance: "operator_set" },
        }),
      ),
    );
    await waitFor(() => expect(onWorkspaceUpdated).toHaveBeenCalledWith(updated));
  });
});

describe("RequirementsCard — Reach/Record reflect the REAL llm_cred binding, not the wizard's PowerSource", () => {
  const profile: WorkspaceProfile = {
    required_secrets: [{ name: "DATABASE_URL", kind: "postgres" }],
    egress_domains: ["registry.npmjs.org"],
  };

  it("treats an unbound workspace as the server default — Record a session stays enabled", async () => {
    render(
      <RequirementsCard
        ws={ws({ profile: profile as unknown as Record<string, unknown> })}
        storedSecretNames={[]}
        onWorkspaceUpdated={vi.fn()}
        onSecretStored={vi.fn()}
      />,
    );
    // Reach's power card states the resolution up front…
    expect(within(screen.getByTestId("power-source-card")).getByText("server default")).toBeInTheDocument();
    // …and Record (last tab) offers the agent session.
    await openTab("Verify");
    expect(screen.getByRole("button", { name: "Verify with a session" })).toBeEnabled();
    expect(screen.queryByText(RD2.RECORD_NEEDS)).not.toBeInTheDocument();
  });

  it("Reach seeds the 'from model access' row from a pinned Integration binding", async () => {
    render(
      <RequirementsCard
        ws={ws({
          profile: profile as unknown as Record<string, unknown>,
          llm_cred: { integration_ref: "ai-acme-key" },
        })}
        storedSecretNames={[]}
        onWorkspaceUpdated={vi.fn()}
        onSecretStored={vi.fn()}
      />,
    );
    const egressGroup = within(screen.getByTestId("group-reach"));
    const chip = egressGroup.getByText("from model access");
    expect(chip).toHaveAttribute("title", RD2.EGRESS_TIP);
    // A pinned binding is named plainly by its Integration ref rather than a
    // fabricated hostname — see step-requirements.tsx's ResolvedToken. (The
    // SAME label also appears in the Model access chip above — scope to the
    // egress group so the two don't collide.)
    expect(egressGroup.getByText("ai-acme-key")).toBeInTheDocument();
  });
});
