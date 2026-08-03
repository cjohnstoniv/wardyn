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

import { RequirementsCard } from "./requirements-card";

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

  it("flags a broken api_key binding whose secret isn't stored", () => {
    render(
      <RequirementsCard
        ws={ws({ llm_cred: { mode: "api_key", api_key_secret: "missing-key" } })}
        storedSecretNames={[]}
        onWorkspaceUpdated={vi.fn()}
        onSecretStored={vi.fn()}
      />,
    );
    expect(screen.getByText(/isn't in the store/i)).toBeInTheDocument();
    expect(screen.getByText("missing-key")).toBeInTheDocument();
  });

  it("opens the model access dialog and reports the saved workspace", async () => {
    const onWorkspaceUpdated = vi.fn();
    setWorkspaceLLMCredMock.mockResolvedValue(ws({ llm_cred: { mode: "managed" } }));
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    render(
      <RequirementsCard ws={ws()} storedSecretNames={[]} onWorkspaceUpdated={onWorkspaceUpdated} onSecretStored={vi.fn()} />,
    );
    await user.click(screen.getByRole("button", { name: /bind model access/i }));
    await user.click(screen.getByRole("radio", { name: /managed/i }));
    await user.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(setWorkspaceLLMCredMock).toHaveBeenCalledWith("ws-1", { mode: "managed" }));
    await waitFor(() => expect(onWorkspaceUpdated).toHaveBeenCalled());
  });
});

describe("RequirementsCard — container workspaces skip the contract, keep model access", () => {
  it("shows the image-is-the-environment note instead of contract groups", () => {
    render(
      <RequirementsCard
        ws={ws({ kind: "container", source: "ubuntu:24.04", ref: undefined })}
        storedSecretNames={[]}
        onWorkspaceUpdated={vi.fn()}
        onSecretStored={vi.fn()}
      />,
    );
    expect(screen.getByText(/image is the environment/i)).toBeInTheDocument();
    expect(screen.getByText("Model access")).toBeInTheDocument();
    expect(screen.queryByText("Network egress")).not.toBeInTheDocument();
  });
});

describe("RequirementsCard — reuses the wizard's StepRequirements and persists edits immediately", () => {
  const profile: WorkspaceProfile = {
    required_secrets: [{ name: "DATABASE_URL", kind: "postgres" }],
    egress_domains: ["registry.npmjs.org"],
  };

  it("renders the same contract groups the wizard renders", () => {
    render(
      <RequirementsCard
        ws={ws({ profile: profile as unknown as Record<string, unknown> })}
        storedSecretNames={[]}
        onWorkspaceUpdated={vi.fn()}
        onSecretStored={vi.fn()}
      />,
    );
    expect(screen.getByTestId("group-secrets")).toBeInTheDocument();
    expect(screen.getByTestId("group-egress")).toBeInTheDocument();
    expect(screen.getByText("DATABASE_URL")).toBeInTheDocument();
    expect(screen.getByText("registry.npmjs.org")).toBeInTheDocument();
  });

  it("flipping a lane calls setRequirements immediately (no separate save step)", async () => {
    const onWorkspaceUpdated = vi.fn();
    const updated = ws({ profile: profile as unknown as Record<string, unknown> });
    setRequirementsMock.mockResolvedValue(updated);
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    render(
      <RequirementsCard
        ws={ws({ profile: profile as unknown as Record<string, unknown> })}
        storedSecretNames={["DATABASE_URL"]}
        onWorkspaceUpdated={onWorkspaceUpdated}
        onSecretStored={vi.fn()}
      />,
    );
    const secretsGroup = within(screen.getByTestId("group-secrets"));
    await user.click(secretsGroup.getByRole("radio", { name: "Optional" }));
    await waitFor(() =>
      expect(setRequirementsMock).toHaveBeenCalledWith(
        "ws-1",
        expect.objectContaining({
          "secret:DATABASE_URL": { level: "optional", provenance: "operator_set" },
        }),
      ),
    );
    await waitFor(() => expect(onWorkspaceUpdated).toHaveBeenCalledWith(updated));
  });
});
