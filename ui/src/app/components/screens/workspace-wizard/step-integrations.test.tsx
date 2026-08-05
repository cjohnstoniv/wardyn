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
vi.mock("../../../lib/api/integrations", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/integrations")>(
    "../../../lib/api/integrations",
  );
  return { ...actual, integrationsApi: { list: (...a: unknown[]) => listIntegrationsMock(...a) } };
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
  listIntegrationsMock.mockReset().mockResolvedValue({
    ai: [{ id: "ai-anthropic", name: "Anthropic API key", typeLabel: "anthropic · api key" }],
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
    await waitFor(() =>
      expect(screen.getAllByRole("button", { name: /use in this workspace/i }).length).toBe(2),
    );
  });
});
