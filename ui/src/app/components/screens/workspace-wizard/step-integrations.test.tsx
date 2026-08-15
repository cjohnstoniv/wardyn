/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Step ③ Integrations: ALL categories are selectable — AI providers and SCM
// hosts included (genericIntegrations deliberately excludes those two, which
// is exactly why the old Reach-tab-only section was invisible on a stack
// whose only integration was an AI provider). Naming one writes an
// integration:<id> row into the same requirements map step ④ renders.
import * as React from "react";
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { WorkspaceRequirementsMap } from "./wizard-types";

const listIntegrationsMock = vi.fn();
const adoptIntegrationMock = vi.fn();
vi.mock("../../../lib/api/integrations", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/integrations")>(
    "../../../lib/api/integrations",
  );
  return {
    ...actual,
    integrationsApi: {
      list: (...a: unknown[]) => listIntegrationsMock(...a),
      adoptIntegration: (...a: unknown[]) => adoptIntegrationMock(...a),
    },
  };
});

import { StepIntegrations } from "./step-integrations";

function Harness({ initial = {} }: { initial?: WorkspaceRequirementsMap }) {
  const [reqs, setReqs] = React.useState<WorkspaceRequirementsMap>(initial);
  return (
    <StepIntegrations
      status={null}
      requirements={reqs}
      setLane={(key, level) => setReqs((r) => ({ ...r, [key]: { level, provenance: "operator_set" } }))}
      clear={(key) =>
        setReqs((r) => {
          const next = { ...r };
          delete next[key];
          return next;
        })
      }
    />
  );
}

beforeEach(() => {
  adoptIntegrationMock.mockReset().mockResolvedValue(undefined);
  listIntegrationsMock.mockReset().mockResolvedValue({
    ai: [
      { id: "ai-anthropic", name: "Anthropic API key", typeLabel: "anthropic · api key" },
      // A DERIVED legacy row: carries its SERVER id, adoptable on first use.
      {
        id: "ai:anthropic_subscription:managed",
        serverId: "anthropic_subscription:managed",
        name: "Claude subscription (managed)",
        typeLabel: "anthropic · managed login",
      },
    ],
    scm: [{ id: "scm-ghes", name: "GHES", typeLabel: "ghes.corp.internal" }],
  });
});

describe("StepIntegrations", () => {
  it("lists AI and SCM rows and names one as integration:<id> required on 'Use in this workspace'", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    render(<Harness />);

    expect(await screen.findByText("Anthropic API key")).toBeInTheDocument();
    expect(screen.getByText("GHES")).toBeInTheDocument();

    const useButtons = screen.getAllByRole("button", { name: /use in this workspace/i });
    await user.click(useButtons[0]);
    // Named: the row grows the Required/Optional lanes + the Not used escape.
    expect(await screen.findByRole("radio", { name: "Required", checked: true })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /not used/i })).toBeInTheDocument();
  });

  it("'Not used' removes the row from the contract", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    render(<Harness initial={{ "integration:ai-anthropic": { level: "required", provenance: "operator_set" } }} />);

    await screen.findByRole("button", { name: /not used/i });
    await user.click(screen.getByRole("button", { name: /not used/i }));
    // All three rows (slug ai + adoptable subscription + scm) offer the toggle.
    await waitFor(() =>
      expect(screen.getAllByRole("button", { name: /use in this workspace/i }).length).toBe(3),
    );
  });

  it("a derived legacy row ADOPTS on first use, then names its SERVER id — an explicit act, removable", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    render(<Harness />);
    expect(await screen.findByText("Claude subscription (managed)")).toBeInTheDocument();
    // Every row with a server identity is a choice — including the derived
    // subscription row (connection status alone never writes a contract row).
    const useButtons = screen.getAllByRole("button", { name: /use in this workspace/i });
    expect(useButtons).toHaveLength(3);

    // Click the subscription row's button (it renders last in the ai list).
    await user.click(useButtons[1]);
    await waitFor(() => expect(adoptIntegrationMock).toHaveBeenCalledWith("anthropic_subscription:managed"));
    // Named under the SERVER id — and removable like any other.
    expect(await screen.findByRole("button", { name: /not used/i })).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: /not used/i }));
    await waitFor(() =>
      expect(screen.getAllByRole("button", { name: /use in this workspace/i })).toHaveLength(3),
    );
  });
});

// ui-wsWizard-6: loading, a failed fetch, and a genuinely-empty account used
// to all render the identical "Nothing connected yet?" copy — the operator
// couldn't tell an in-flight request from a dead one from an empty account.
describe("StepIntegrations — loading/error are distinct from the true-empty state (ui-wsWizard-6)", () => {
  it("shows a loading state before the fetch resolves, not the empty-account copy", async () => {
    let resolveList!: (v: { ai: never[]; scm: never[] }) => void;
    listIntegrationsMock.mockReset().mockReturnValue(new Promise((res) => (resolveList = res)));
    render(<Harness />);

    expect(screen.getByTestId("integrations-loading")).toBeInTheDocument();
    expect(screen.queryByText(/nothing connected yet/i)).not.toBeInTheDocument();

    resolveList({ ai: [], scm: [] });
    expect(await screen.findByText(/nothing connected yet/i)).toBeInTheDocument();
    expect(screen.queryByTestId("integrations-loading")).not.toBeInTheDocument();
  });

  it("shows a retryable error on a failed fetch, not the empty-account copy — and Retry recovers", async () => {
    listIntegrationsMock.mockReset().mockRejectedValueOnce(new Error("network down"));
    render(<Harness />);

    expect(await screen.findByTestId("integrations-error")).toBeInTheDocument();
    expect(screen.queryByText(/nothing connected yet/i)).not.toBeInTheDocument();

    listIntegrationsMock.mockResolvedValueOnce({
      ai: [{ id: "ai-anthropic", name: "Anthropic API key", typeLabel: "anthropic · api key" }],
      scm: [],
    });
    await userEvent.click(screen.getByRole("button", { name: /retry/i }));
    expect(await screen.findByText("Anthropic API key")).toBeInTheDocument();
    expect(screen.queryByTestId("integrations-error")).not.toBeInTheDocument();
  });
});