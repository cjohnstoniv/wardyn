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
import { act, render, screen, waitFor } from "@testing-library/react";
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
vi.mock("../../../lib/api/policy-preview", () => ({ previewRunPolicy: vi.fn().mockResolvedValue({ spec: {}, pending: [], warnings: [], repository_access: [] }) }));
vi.mock("../../../lib/api/runs", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../../lib/api/runs")>();
  return {
    ...actual,
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
const listWorkspacesMock = vi.fn();
vi.mock("../../../lib/api/workspaces", () => ({
  workspaces: { listWorkspaces: (...a: unknown[]) => listWorkspacesMock(...a) },
}));
vi.mock("../../../lib/capabilities", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/capabilities")>("../../../lib/capabilities");
  return { ...actual, useMyCapabilities: () => null };
});
// The REAL rail with its props recorded — pins the SEAM (what the screen
// hands the rail), same pattern new-run-screen-launch.test.tsx uses for
// launch.credentialRefused/refusedProvider.
const railProps: Array<{
  modelProvider?: { selectedId?: string; changeNote: string | null };
  launch: { problem: string | null; issue?: { panel: string } | null; workspaceUnavailable?: boolean };
}> = [];
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
import { setField } from "../../../../test/set-field";
import { DENIED } from "../../../lib/permissions-copy";

const { bedrock, claude, anthropicKey, gateway } = MODEL_PROVIDERS;
const user = userEvent.setup({ pointerEventsCheck: 0 });

function renderScreen() {
  return render(
    <MemoryRouter>
      <OperatorProvider principal="test-owner" operator>
        <NewRunScreen />
      </OperatorProvider>
    </MemoryRouter>,
  );
}

// F2 (#612) — seeds state.workspaces the same way B4b's clone door does
// (navigate("/runs/new", { state: { prefill } })), the shortest path to an
// attached workspace without driving the Add-workspace dialog through the UI.
function renderScreenWithWorkspace(workspaceId: string) {
  return render(
    <MemoryRouter
      initialEntries={[
        { pathname: "/runs/new", state: { prefill: { inlinePolicy: false, state: { workspaces: [{ workspaceId }] } } } },
      ]}
    >
      <OperatorProvider principal="test-owner" operator>
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
  listWorkspacesMock.mockReset().mockResolvedValue([]);
});

describe("NewRunScreen — R1: the sole candidate is sent even with no picker", () => {
  it("sends model_provider on the wire", async () => {
    getSetupStatusMock.mockResolvedValue(providerStatus([{ provider: bedrock, defaultFor: ["claude-code"], state: "live" }]));
    renderScreen();
    setField(await screen.findByLabelText("Title"), "Refund flow");
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
    setField(await screen.findByLabelText("Title"), "Refund flow");
    await waitFor(() => expect(railProps.at(-1)?.modelProvider?.selectedId).toBe(gateway.id));
    await user.click(screen.getByRole("button", { name: /Launch run/ }));
    await waitFor(() => expect(createRunMock).toHaveBeenCalled());
    expect(createRunMock.mock.calls[0][0].model_provider).toBe(gateway.id);
  });
});

describe("NewRunScreen — F2 (#612): the primary workspace's pin beats the roster default", () => {
  it("sends the pinned provider's id on the wire, not the admin default's", async () => {
    getSetupStatusMock.mockResolvedValue(
      providerStatus([
        { provider: gateway, defaultFor: ["claude-code"], state: "live" },
        { provider: claude, state: "live" },
      ]),
    );
    listWorkspacesMock.mockResolvedValue([
      {
        id: "ws1",
        name: "repo-a",
        kind: "local_dir",
        source: "/home/agent/repo-a",
        status: "scanned",
        llm_cred: { provider_ref: claude.id },
      },
    ]);
    renderScreenWithWorkspace("ws1");
    setField(await screen.findByLabelText("Title"), "Refund flow");
    await waitFor(() => expect(railProps.at(-1)?.modelProvider?.selectedId).toBe(claude.id));
    await user.click(screen.getByRole("button", { name: /Launch run/ }));
    await waitFor(() => expect(createRunMock).toHaveBeenCalled());
    expect(createRunMock.mock.calls[0][0].model_provider).toBe(claude.id);
  });

  // Opus review round 2 (F2) — the ORIGINAL bug, reproduced with a REAL race:
  // providers resolve first, this effect automatically adopts the roster
  // default (gateway), and ONLY THEN does the workspace list resolve with the
  // pin. The old "keep any still-serving previousId" rule would have let that
  // automatic gateway pick outlive the pin forever; previousExplicit fixes it.
  it("a pin that resolves ~300ms AFTER the automatic default pick still wins", async () => {
    getSetupStatusMock.mockResolvedValue(
      providerStatus([
        { provider: gateway, defaultFor: ["claude-code"], state: "live" },
        { provider: claude, state: "live" },
      ]),
    );
    listWorkspacesMock.mockImplementation(
      () =>
        new Promise((resolve) =>
          setTimeout(
            () =>
              resolve([
                {
                  id: "ws1",
                  name: "repo-a",
                  kind: "local_dir",
                  source: "/home/agent/repo-a",
                  status: "scanned",
                  llm_cred: { provider_ref: claude.id },
                },
              ]),
            300,
          ),
        ),
    );
    renderScreenWithWorkspace("ws1");
    // The pin arrives and wins outright.
    await waitFor(() => expect(railProps.at(-1)?.modelProvider?.selectedId).toBe(claude.id), { timeout: 2000 });
    // The automatic default landed first — proves the race is real, not
    // avoided by mock timing. Read from the render history rather than
    // polled live: a slow runner can still be busy when the pin lands, and
    // the default's window has closed by the time a live poll looks.
    expect(railProps.some((p) => p.modelProvider?.selectedId === gateway.id)).toBe(true);
    setField(await screen.findByLabelText("Title"), "Refund flow");
    await user.click(screen.getByRole("button", { name: /Launch run/ }));
    await waitFor(() => expect(createRunMock).toHaveBeenCalled());
    expect(createRunMock.mock.calls[0][0].model_provider).toBe(claude.id);
  });

  // Conductor follow-up (train 21 gap) — `explicitPick.current = true` in
  // onModelProviderChange has no screen-level coverage; only the pure lane
  // unit test exercises the rule. This proves it end to end: the automatic
  // default lands, the PERSON overrides it, and a THIRD candidate's pin
  // arriving afterward must not un-pick the person's own choice — pin beats
  // an AUTOMATIC previousId (the earlier test above), but an EXPLICIT one
  // always outranks the pin (model-provider-lane.ts's doc comment).
  it("an explicit pick survives a workspace pin that arrives after it", async () => {
    getSetupStatusMock.mockResolvedValue(
      providerStatus([
        { provider: gateway, defaultFor: ["claude-code"], state: "live" },
        { provider: claude, state: "live" },
        { provider: anthropicKey, state: "live" },
      ]),
    );
    // The test, not the clock, decides when the workspace list resolves: on a
    // slow runner a fixed 800ms timer fired before step 1 could observe the
    // automatic default, and the pin (correctly) replaced it first.
    let resolveWorkspaces: (value: unknown) => void = () => {};
    listWorkspacesMock.mockImplementation(
      () =>
        new Promise((resolve) => {
          resolveWorkspaces = resolve;
        }),
    );
    renderScreenWithWorkspace("ws1");
    setField(await screen.findByLabelText("Title"), "Refund flow");
    // 1. The roster default is auto-selected first.
    await waitFor(() => expect(railProps.at(-1)?.modelProvider?.selectedId).toBe(gateway.id));

    // 2. The person explicitly picks a DIFFERENT candidate.
    await user.click(screen.getByRole("combobox", { name: RAIL_PROVIDER.LABEL }));
    await user.click(await screen.findByRole("option", { name: RAIL_PROVIDER.OPTION("Claude subscription", "sign-in", "signed in") }));
    await waitFor(() => expect(railProps.at(-1)?.modelProvider?.selectedId).toBe(claude.id));

    // 3. Only now does the workspace resolve, pinning a THIRD candidate the
    // person never touched. Resolving it and letting its effects run proves the
    // pin actually arrived and was still overridden, not merely that it never
    // got a chance to run.
    await act(async () => {
      resolveWorkspaces([
        {
          id: "ws1",
          name: "repo-a",
          kind: "local_dir",
          source: "/home/agent/repo-a",
          status: "scanned",
          llm_cred: { provider_ref: anthropicKey.id }, // a THIRD candidate, never picked by the person
        },
      ]);
      await new Promise((resolve) => setTimeout(resolve, 100));
    });

    // 4. The explicit pick stands — never displaced by the late pin.
    expect(railProps.at(-1)?.modelProvider?.selectedId).toBe(claude.id);
    await user.click(screen.getByRole("button", { name: /Launch run/ }));
    await waitFor(() => expect(createRunMock).toHaveBeenCalled());
    expect(createRunMock.mock.calls[0][0].model_provider).toBe(claude.id);
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
    setField(await screen.findByLabelText("Title"), "Refund flow");
    await waitFor(() => expect(screen.getByRole("combobox", { name: RAIL_PROVIDER.LABEL })).toBeInTheDocument());
    expect(await screen.findByText(RAIL_PROVIDER.LAUNCH_HINT)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Launch run/ })).toBeDisabled();

    await user.click(screen.getByRole("combobox", { name: RAIL_PROVIDER.LABEL }));
    await user.click(await screen.findByRole("option", { name: RAIL_PROVIDER.OPTION("Anthropic API key", "key", "not available") }));

    expect(screen.queryByText(RAIL_PROVIDER.LAUNCH_HINT)).toBeNull();
    expect(screen.getByRole("button", { name: /Launch run/ })).toBeEnabled();
    await user.click(screen.getByRole("button", { name: /Launch run/ }));
    await waitFor(() => expect(createRunMock).toHaveBeenCalled());
    expect(createRunMock.mock.calls[0][0].model_provider).toBe(anthropicKey.id);
  });
});

// #1052 — R5b: the server's own providers_ungranted fact (SetupHarnessTool),
// end to end from /setup/status to the rail's NOT_GRANTED sentence and a
// disabled Launch. No provider block reaches this test at all (model_providers
// stays empty, as it always is for R5b — capVisible already narrowed it away)
// — `harnesses` alone carries the signal.
describe("NewRunScreen — R5b (#1052): providers_ungranted disables Launch", () => {
  it("names NOT_GRANTED and disables Launch", async () => {
    getSetupStatusMock.mockResolvedValue(
      providerStatus([], {
        harnesses: [
          {
            id: "claude-code",
            display: "Claude Code",
            has_gateway: true,
            has_login: true,
            enabled: true,
            providers_ungranted: true,
          },
        ],
      }),
    );
    renderScreen();
    setField(await screen.findByLabelText("Title"), "Refund flow");
    expect(await screen.findByText(RAIL_PROVIDER.NOT_GRANTED("Claude Code"))).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Launch run/ })).toBeDisabled();
    expect(screen.queryByRole("combobox", { name: RAIL_PROVIDER.LABEL })).toBeNull();
  });
});

// R6 rule (3): a workspace pin already answers "why wait" its own way (the
// server refuses a pin it cannot serve, by name), so LAUNCH_HINT stays silent
// whenever one is set — even a pin naming no candidate, which preselects
// nothing.
describe("NewRunScreen — R6 rule (3): a workspace pin silences LAUNCH_HINT", () => {
  it("names no pick-a-provider problem under a pin that is not a candidate", async () => {
    getSetupStatusMock.mockResolvedValue(
      providerStatus([
        { provider: gateway, state: "not_configured" },
        { provider: anthropicKey, state: "not_configured" },
      ]),
    );
    listWorkspacesMock.mockResolvedValue([
      {
        id: "ws1",
        name: "repo-a",
        kind: "local_dir",
        source: "/home/agent/repo-a",
        status: "scanned",
        llm_cred: { provider_ref: claude.id },
      },
    ]);
    renderScreenWithWorkspace("ws1");
    await waitFor(() => expect(screen.getByRole("combobox", { name: RAIL_PROVIDER.LABEL })).toBeInTheDocument());
    // The pinned workspace is its own issue (#1922); the pick-a-provider hint
    // is never the reason named, with the required title given or not.
    setField(screen.getByLabelText("Title"), "Pinned workspace");
    await waitFor(() => expect(railProps.at(-1)?.launch.problem).toBe(DENIED.WORKSPACE_NOT_AVAILABLE));
    expect(railProps.at(-1)?.launch.issue?.panel).toBe("workspace");
    expect(railProps.at(-1)?.modelProvider?.selectedId).toBeUndefined();
    expect(screen.queryByText(RAIL_PROVIDER.LAUNCH_HINT)).toBeNull();
  });
});

// #1018: a pin the server hid (llm_cred.provider_unavailable) is still a pin
// to a provider this person can't use — the rail preselects nothing, never
// the roster default, so nothing is substituted, and Launch is refused.
describe("NewRunScreen — #1018: a hidden pin preselects nothing", () => {
  it("does not adopt the roster default under provider_unavailable", async () => {
    getSetupStatusMock.mockResolvedValue(
      providerStatus([
        { provider: gateway, defaultFor: ["claude-code"], state: "live" },
        { provider: claude, state: "live" },
      ]),
    );
    listWorkspacesMock.mockResolvedValue([
      {
        id: "ws1",
        name: "repo-a",
        kind: "local_dir",
        source: "/home/agent/repo-a",
        status: "scanned",
        llm_cred: { provider_unavailable: true },
      },
    ]);
    renderScreenWithWorkspace("ws1");
    setField(await screen.findByLabelText("Title"), "Refund flow");
    await waitFor(() => expect(screen.getByRole("combobox", { name: RAIL_PROVIDER.LABEL })).toBeInTheDocument());
    await waitFor(() => expect(railProps.at(-1)?.launch.workspaceUnavailable).toBe(true));
    expect(railProps.at(-1)?.modelProvider?.selectedId).toBeUndefined();
    expect(createRunMock).not.toHaveBeenCalled();
  });
});

describe("NewRunScreen — R7/R8: switching the Agent field re-resolves the provider", () => {
  it("R8: Corp gateway serves both agents — switching to Codex CLI keeps it, silently", async () => {
    getSetupStatusMock.mockResolvedValue(
      providerStatus([{ provider: gateway, defaultFor: ["claude-code", "codex-cli"], state: "live" }]),
    );
    renderScreen();
    setField(await screen.findByLabelText("Title"), "Refund flow");
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
    setField(await screen.findByLabelText("Title"), "Refund flow");
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
    setField(await screen.findByLabelText("Title"), "Nightly cleanup");
    await user.click(screen.getByRole("radio", { name: "Shell command" }));
    setField(screen.getByLabelText("Command"), "echo hi");
    await user.click(screen.getByRole("button", { name: /Launch run/ }));
    await waitFor(() => expect(createRunMock).toHaveBeenCalled());
    expect(createRunMock.mock.calls[0][0].model_provider).toBeUndefined();
  });
});

// An install with no provider block carries no model_providers key at all
// (omitzero). That must read as no block: no picker, no gate, and Launch
// still works.
describe("NewRunScreen — a legacy install's absent model_providers", () => {
  it("renders and launches with no model_provider on the wire", async () => {
    getSetupStatusMock.mockResolvedValue({ ...baseStatus(), model_providers: undefined });
    renderScreen();
    setField(await screen.findByLabelText("Title"), "Legacy null block");
    await user.click(screen.getByRole("button", { name: /Launch run/ }));
    await waitFor(() => expect(createRunMock).toHaveBeenCalled());
    expect(createRunMock.mock.calls[0][0].model_provider).toBeUndefined();
  });
});
