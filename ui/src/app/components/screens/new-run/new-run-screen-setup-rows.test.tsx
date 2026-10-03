/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// f-f4 — preflight's setup rows on the rail, and a `missing` backend row
// blocking Launch only while its verdict is current.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

const getSetupStatusMock = vi.fn();
vi.mock("../../../lib/api/setup", () => ({
  setup: { getSetupStatus: (...a: unknown[]) => getSetupStatusMock(...a) },
}));
vi.mock("../../../lib/api/policies", () => ({
  policies: {
    listPolicies: () => Promise.resolve([]),
    createPolicy: vi.fn(),
    getDefaultPolicy: () => Promise.resolve({ min_confinement_class: "CC1" }),
  },
}));
const preflightRunMock = vi.fn();
vi.mock("../../../lib/api/runs", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../../lib/api/runs")>();
  return {
    isCredentialRefusal: actual.isCredentialRefusal,
    isGitCredentialRefusal: actual.isGitCredentialRefusal,
    runs: {
      createRun: vi.fn(),
      listRuns: () => Promise.resolve([]),
      preflightRun: (...a: unknown[]) => preflightRunMock(...a),
      gradePolicy: () => Promise.resolve({ risk_assessment: [], overall_risk: "low" }),
    },
  };
});
vi.mock("../../../lib/api/workspaces", () => ({
  workspaces: { listWorkspaces: () => Promise.resolve([]) },
}));
vi.mock("../../../lib/capabilities", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/capabilities")>("../../../lib/capabilities");
  return { ...actual, useMyCapabilities: () => null };
});

import { NewRunScreen } from "./new-run-screen";
import { baseStatus } from "../../../lib/test-fixtures";
import { OperatorProvider } from "../../wardyn/operator-context";
import { NO_BARRIER, RAIL_SETUP } from "../../wardyn/copy";
import type { SetupItem } from "../../../lib/types/runs";

const user = userEvent.setup({ pointerEventsCheck: 0 });

const BACKEND = (status: string): SetupItem => ({
  id: "backend:CC2",
  kind: "backend",
  label: "Sandbox barrier: Wall",
  required_by: "the proposal's confinement class",
  status,
  detail: "no Wall (gVisor) runtime registered on this host yet",
});
const LLM_MISSING: SetupItem = {
  id: "llm_access:codex-cli",
  kind: "llm_access",
  label: "Model access for codex-cli",
  required_by: "the agent",
  status: "missing",
  detail: "no model provider serves it on this deployment",
};

async function preflighted(items: SetupItem[], classes: Array<"CC1" | "CC2" | "CC3"> = ["CC1"]) {
  getSetupStatusMock.mockResolvedValue(
    baseStatus({ runner: { driver: "docker", confinement_classes: classes } }),
  );
  preflightRunMock.mockResolvedValue({
    setup_items: items,
    enforced_confinement_class: "CC2",
    overall_risk: "low",
    warnings: [],
  });
  render(
    <MemoryRouter>
      <OperatorProvider operator>
        <NewRunScreen />
      </OperatorProvider>
    </MemoryRouter>,
  );
  await user.type(await screen.findByLabelText("Title"), "Refund flow");
  await user.click(screen.getByRole("button", { name: /^Check again$/ }));
  await screen.findByTestId("preflight-result");
}

beforeEach(() => {
  getSetupStatusMock.mockReset().mockResolvedValue(baseStatus());
  preflightRunMock.mockReset();
});

describe("NewRunScreen — preflight's setup rows", () => {
  it("a missing backend row shows the server's text and blocks Launch, with the barrier CTA", async () => {
    await preflighted([BACKEND("missing")]);
    expect(screen.getByText(RAIL_SETUP.HEADING)).toBeInTheDocument();
    expect(screen.getByText(/Sandbox barrier: Wall — no Wall \(gVisor\) runtime/)).toBeInTheDocument();
    expect(screen.getByText(RAIL_SETUP.BACKEND_BLOCK, { exact: false })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: NO_BARRIER.CTA })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Launch run/ })).toBeDisabled();
  });

  it("a host with no barrier at all keeps one sentence: the rail's own line, not a second backend block", async () => {
    await preflighted([BACKEND("missing")], []);
    expect(screen.getAllByRole("link", { name: NO_BARRIER.CTA })).toHaveLength(1);
    expect(screen.queryByText(RAIL_SETUP.BACKEND_BLOCK, { exact: false })).toBeNull();
    expect(screen.getByRole("button", { name: /Launch run/ })).toBeDisabled();
  });

  it("a stale verdict (body changed since) neither shows nor blocks", async () => {
    await preflighted([BACKEND("missing")]);
    await user.type(screen.getByLabelText("Title"), " again");
    expect(screen.queryByText(RAIL_SETUP.BACKEND_BLOCK, { exact: false })).toBeNull();
    expect(screen.getByRole("button", { name: /Launch run/ })).toBeEnabled();
  });

  it("an unverified backend row is shown but does not block", async () => {
    await preflighted([BACKEND("unverified")]);
    expect(screen.getByText(/Sandbox barrier: Wall/)).toBeInTheDocument();
    expect(screen.queryByText(RAIL_SETUP.BACKEND_BLOCK, { exact: false })).toBeNull();
    expect(screen.getByRole("button", { name: /Launch run/ })).toBeEnabled();
  });

  it("a missing llm_access row is advisory and never blocks an interactive run", async () => {
    await preflighted([LLM_MISSING]);
    expect(screen.getByText(/Model access for codex-cli — no model provider/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Launch run/ })).toBeEnabled();
  });

  it("satisfied rows stay hidden", async () => {
    await preflighted([BACKEND("satisfied")]);
    expect(screen.queryByText(RAIL_SETUP.HEADING)).toBeNull();
  });
});
