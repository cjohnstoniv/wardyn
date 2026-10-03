/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// f-f5: an unattended agent run with no reachable model waits at Launch; an
// interactive one stays advisory. The verdict is the current body's own
// preflight llm_access row, not the mount-time llm_ready.
import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

vi.mock("../../../lib/api/health", () => ({
  health: { health: () => Promise.resolve({ components: {} }) },
}));

import { NewRunLaunchPanel, type NewRunLaunchPanelProps } from "./new-run-launch-panel";
import { RAIL_MODEL_ACCESS } from "../../wardyn/model-access-copy";
import type { PreflightResult } from "../../../lib/types";

const llm = (status: string): PreflightResult => ({
  enforced_confinement_class: "CC1",
  setup_items: [
    { id: "llm_access:codex-cli", kind: "llm_access", label: "Model access", required_by: "run", status },
  ],
});

function renderPanel(over: Partial<NewRunLaunchPanelProps>) {
  const props: NewRunLaunchPanelProps = {
    governanceProfile: undefined,
    savedPolicy: undefined,
    cc: "CC1",
    showModelWarning: false,
    toolRules: null,
    pushRules: undefined,
    unattended: true,
    isInteractive: false,
    interactiveStart: "agent",
    agent: "codex-cli",
    toolApprovals: "hold",
    onLaunch: () => {},
    launchDisabled: false,
    launchSpinning: false,
    launching: false,
    error: null,
    errorSeq: 0,
    credentialRefused: false,
    refusedProvider: undefined,
    launchBody: null,
    onPreflight: async () => {},
    preflightRefusal: null,
    noBarrier: false,
    mode: "batch",
    task: "do it",
    useSaved: false,
    specParsedOk: true,
    selectedPolicyId: undefined,
    policiesLoaded: true,
    pin: undefined,
    workspaces: [],
    selectedWorkspaceId: undefined,
    caps: null,
    modelProviders: [],
    preflightIsCurrent: true,
    preflightError: null,
    preflightErrorSeq: 0,
    preflightResult: llm("missing"),
    agentRow: undefined,
    isAgent: true,
    providerCandidates: [],
    providerAccess: [],
    selectedModelProviderId: undefined,
    onModelProviderChange: () => {},
    providerChangeNote: null,
    providerGateState: undefined,
    agentName: "codex-cli",
    adoDialog: {
      open: false,
      connecting: false,
      org: "",
      blockedUrl: null,
      onConfirm: () => {},
      onFallbackClick: () => {},
      onCancel: () => {},
    },
    ...over,
  };
  return render(
    <MemoryRouter>
      <NewRunLaunchPanel {...props} />
    </MemoryRouter>,
  );
}

const block = RAIL_MODEL_ACCESS.UNATTENDED_BLOCK("codex-cli");
const launch = () => screen.getByRole("button", { name: /Launch run/ });

describe("NewRunLaunchPanel — no model for an unattended agent run", () => {
  it("blocks Launch and says why, with the Connect link", () => {
    renderPanel({ showModelWarning: true });
    expect(launch()).toBeDisabled();
    expect(screen.getByText(block, { exact: false })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: RAIL_MODEL_ACCESS.NO_PROVIDER_CTA })).toBeInTheDocument();
    expect(screen.queryByText(RAIL_MODEL_ACCESS.NO_PROVIDER, { exact: false })).toBeNull();
  });

  it("an earlier problem arm keeps its own sentence and no Connect link", () => {
    renderPanel({ task: "" });
    expect(screen.getByText("An autonomous run needs a task to perform.", { exact: false })).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: RAIL_MODEL_ACCESS.NO_PROVIDER_CTA })).toBeNull();
    expect(launch()).toBeDisabled();
  });

  it("an interactive body keeps the advice and is not blocked", () => {
    renderPanel({ isInteractive: true, mode: "interactive", unattended: false, showModelWarning: true });
    expect(launch()).toBeEnabled();
    expect(screen.queryByText(block, { exact: false })).toBeNull();
    expect(screen.getByText(RAIL_MODEL_ACCESS.NO_PROVIDER, { exact: false })).toBeInTheDocument();
  });

  it("a ready provider lets Launch through", () => {
    renderPanel({ preflightResult: llm("satisfied") });
    expect(launch()).toBeEnabled();
  });

  it("blocks on the llm_access row even when llm_ready is true (no showModelWarning)", () => {
    renderPanel({ showModelWarning: false });
    expect(launch()).toBeDisabled();
  });

  it("a verdict from an old body never blocks", () => {
    renderPanel({ preflightIsCurrent: false });
    expect(launch()).toBeEnabled();
  });
});
