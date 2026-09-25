/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #542 (design §5.6, packet MP-C) — the screen's half of the provider picker:
// candidate resolution off /setup/status, the defaulting/agent-switch rule
// (model-provider-lane.ts's resolveProviderSelection) actually wired to the
// Agent field, R6's Launch-disable, and the wire body carrying model_provider.
// Its own copy of the screen's mock harness — see new-run-screen-launch.test.tsx's
// header for why every suite duplicates it rather than sharing one module.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
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
const navigateMock = vi.fn();
vi.mock("react-router-dom", async () => {
  const actual = await vi.importActual<typeof import("react-router-dom")>("react-router-dom");
  return { ...actual, useNavigate: () => navigateMock };
});
const createRunMock = vi.fn();
vi.mock("../../../lib/api/runs", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../../lib/api/runs")>();
  return {
    isCredentialRefusal: actual.isCredentialRefusal,
    isGitCredentialRefusal: actual.isGitCredentialRefusal,
    runs: {
      createRun: (...a: unknown[]) => createRunMock(...a),
      listRuns: () => Promise.resolve([]),
      preflightRun: () => Promise.resolve({}),
      gradePolicy: () => Promise.resolve({ risk_assessment: [], overall_risk: "low" }),
    },
  };
});
vi.mock("../../../lib/hooks/use-ado-connect", () => ({
  useAdoConnect: () => ({ connecting: false, connect: vi.fn(), connectFallback: vi.fn(), cancel: vi.fn(), blockedUrl: null }),
}));
vi.mock("../../../lib/api/workspaces", () => ({
  workspaces: { listWorkspaces: () => Promise.resolve([]) },
}));
vi.mock("../../../lib/capabilities", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/capabilities")>("../../../lib/capabilities");
  return { ...actual, useMyCapabilities: () => null };
});
// The REAL rail with its props recorded — pins the SEAM (what the screen
// hands the rail), same pattern new-run-screen-launch.test.tsx uses for
// launch.credentialRefused/refusedProvider.
const railProps: Array<{ modelProvider?: { selectedId?: string; changeNote: string | null } }> = [];
vi.mock("./new-run-rail", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./new-run-rail")>();
  return {
    ...actual,
    RunRail: (props: Parameters<typeof actual.RunRail>[0]) => {
      railProps.push(props);
      return actual.RunRail(props);
    },
  };
});

import { NewRunScreen } from "./new-run-screen";
import { OperatorProvider } from "../../wardyn/operator-context";
import { baseStatus, MODEL_PROVIDERS, providerStatus } from "../../../lib/test-fixtures";
import { RAIL_PROVIDER } from "../../wardyn/copy";

const { bedrock, claude, anthropicKey, gateway } = MODEL_PROVIDERS;
const user = userEvent.setup({ pointerEventsCheck: 0 });

function renderScreen() {
  return render(
    <MemoryRouter>
      <OperatorProvider operator>
        <NewRunScreen />
      </OperatorProvider>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  railProps.length = 0;
  navigateMock.mockReset();
  createRunMock.mockReset().mockResolvedValue({ id: "run_1" });
  getSetupStatusMock.mockReset().mockResolvedValue(baseStatus());
});

describe("NewRunScreen — R1: the sole candidate is sent even with no picker", () => {
  it("sends model_provider on the wire", async () => {
    getSetupStatusMock.mockResolvedValue(providerStatus([{ provider: bedrock, defaultFor: ["claude-code"], state: "live" }]));
    renderScreen();
    await user.type(await screen.findByLabelText("Title"), "Refund flow");
    await waitFor(() => expect(railProps.at(-1)?.modelProvider?.selectedId).toBe(bedrock.id));
    await user.click(screen.getByRole("button", { name: /Launch run/ }));
    await waitFor(() => expect(createRunMock).toHaveBeenCalled());
    expect(createRunMock.mock.calls[0][0].model_provider).toBe(bedrock.id);
  });
});

describe("NewRunScreen — R2: the admin default is preselected among several candidates", () => {
  it("sends the default's id, unprompted", async () => {
    getSetupStatusMock.mockResolvedValue(
      providerStatus([
        { provider: gateway, defaultFor: ["claude-code"], state: "live" },
        { provider: claude, state: "live" },
      ]),
    );
    renderScreen();
    await user.type(await screen.findByLabelText("Title"), "Refund flow");
    await waitFor(() => expect(railProps.at(-1)?.modelProvider?.selectedId).toBe(gateway.id));
    await user.click(screen.getByRole("button", { name: /Launch run/ }));
    await waitFor(() => expect(createRunMock).toHaveBeenCalled());
    expect(createRunMock.mock.calls[0][0].model_provider).toBe(gateway.id);
  });
});

describe("NewRunScreen — R6 (QC-4): no default among several candidates — Launch waits for a choice", () => {
  it("disables Launch with LAUNCH_HINT until the person picks one", async () => {
    getSetupStatusMock.mockResolvedValue(
      providerStatus([
        { provider: gateway, state: "not_configured" },
        { provider: anthropicKey, state: "not_configured" },
      ]),
    );
    renderScreen();
    await user.type(await screen.findByLabelText("Title"), "Refund flow");
    await waitFor(() => expect(screen.getByRole("combobox", { name: RAIL_PROVIDER.LABEL })).toBeInTheDocument());
    expect(await screen.findByText(RAIL_PROVIDER.LAUNCH_HINT)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Launch run/ })).toBeDisabled();

    await user.click(screen.getByRole("combobox", { name: RAIL_PROVIDER.LABEL }));
    await user.click(await screen.findByRole("option", { name: RAIL_PROVIDER.OPTION("Anthropic API key", "key", "not added") }));

    expect(screen.queryByText(RAIL_PROVIDER.LAUNCH_HINT)).toBeNull();
    expect(screen.getByRole("button", { name: /Launch run/ })).toBeEnabled();
    await user.click(screen.getByRole("button", { name: /Launch run/ }));
    await waitFor(() => expect(createRunMock).toHaveBeenCalled());
    expect(createRunMock.mock.calls[0][0].model_provider).toBe(anthropicKey.id);
  });
});

describe("NewRunScreen — R7/R8: switching the Agent field re-resolves the provider", () => {
  it("R8: Corp gateway serves both agents — switching to Codex CLI keeps it, silently", async () => {
    getSetupStatusMock.mockResolvedValue(
      providerStatus([{ provider: gateway, defaultFor: ["claude-code", "codex-cli"], state: "live" }]),
    );
    renderScreen();
    await user.type(await screen.findByLabelText("Title"), "Refund flow");
    await waitFor(() => expect(railProps.at(-1)?.modelProvider?.selectedId).toBe(gateway.id));

    await user.click(screen.getByRole("combobox", { name: "Agent" }));
    await user.click(await screen.findByRole("option", { name: "Codex CLI" }));

    await waitFor(() => expect(railProps.at(-1)?.modelProvider?.selectedId).toBe(gateway.id));
    expect(railProps.at(-1)?.modelProvider?.changeNote).toBeNull();
  });

  it("R7: Anthropic API key doesn't serve Codex CLI — switching names the change", async () => {
    const openaiKey = { id: "openai-key", name: "OpenAI API key", kind: "openai_api_key", harnesses: ["codex-cli"], host: "api.openai.com" };
    getSetupStatusMock.mockResolvedValue(
      providerStatus([
        { provider: anthropicKey, defaultFor: ["claude-code"], state: "live" },
        { provider: openaiKey, defaultFor: ["codex-cli"], state: "live" },
      ]),
    );
    renderScreen();
    await user.type(await screen.findByLabelText("Title"), "Refund flow");
    await waitFor(() => expect(railProps.at(-1)?.modelProvider?.selectedId).toBe(anthropicKey.id));

    await user.click(screen.getByRole("combobox", { name: "Agent" }));
    await user.click(await screen.findByRole("option", { name: "Codex CLI" }));

    await waitFor(() => expect(railProps.at(-1)?.modelProvider?.selectedId).toBe(openaiKey.id));
    expect(railProps.at(-1)?.modelProvider?.changeNote).toBe(
      RAIL_PROVIDER.CHANGED("OpenAI API key", "Anthropic API key", "Codex CLI"),
    );
  });
});

describe("NewRunScreen — a command run never carries a model provider", () => {
  it("omits model_provider even with a provider block configured", async () => {
    getSetupStatusMock.mockResolvedValue(providerStatus([{ provider: bedrock, defaultFor: ["claude-code"], state: "live" }]));
    renderScreen();
    await user.type(await screen.findByLabelText("Title"), "Nightly cleanup");
    await user.click(screen.getByRole("radio", { name: "Shell command" }));
    await user.type(screen.getByLabelText("Command"), "echo hi");
    await user.click(screen.getByRole("button", { name: /Launch run/ }));
    await waitFor(() => expect(createRunMock).toHaveBeenCalled());
    expect(createRunMock.mock.calls[0][0].model_provider).toBeUndefined();
  });
});
